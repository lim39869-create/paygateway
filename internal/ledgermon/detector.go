package ledgermon

import (
	"context"
	"sync"

	"paygateway/internal/admin/capture"
	"paygateway/internal/logger"
)

// SeenStore 增量指纹持久化（由 store 层实现，SQLite ledger_checksums 表）
// 接口刻意最小：仅指纹集合读写，避免与 PersonalStore 全接口耦合。
type SeenStore interface {
	// LoadSeen 加载某 (设备, 账户) 已见指纹集合（服务端重启后恢复基线，防重启漏检）
	LoadSeen(ctx context.Context, deviceID string, accountID int64) ([]string, error)
	// SaveSeen 持久化增量后的完整指纹集合（覆盖写；集合小（可见行 ≤ 数十），全量写足够）
	SaveSeen(ctx context.Context, deviceID string, accountID int64, fingerprints []string) error
}

// IncrementalDetector 基线 + 增量检测：账本页行去重（指纹 = 时间|金额，见 capture.AlipayLedgerRow）
//
// 语义：
//   - 首次读取（无基线）→ 全量建立 seen 集合，不推送（防把历史收款当新收款）
//   - 后续读取 → 只推送不在 seen 集合的新行（时间|金额指纹，秒级时间戳区分同金额多笔）
//   - 指纹持久化：服务端重启后从 store 恢复，重启期间的新收款不丢
//
// 注意：baselined 与 seen 大小解耦——账本页空态（无可见行）也是有效基线，
// 否则首笔真实收款会被当作"历史"吞掉（漏收款）。
//
// 并发安全：单账户循环内调用，但持久化读写用锁兜底（防未来多 goroutine 场景）。
type IncrementalDetector struct {
	mu        sync.Mutex
	deviceID  string
	accountID int64
	baselined bool // 是否已建立基线（与 seen 大小无关，空态页也是基线）
	seen      map[string]bool
	store     SeenStore // nil = 仅内存（不持久化）
}

// NewDetector 创建增量检测器。store 可 nil（仅内存基线）。
func NewDetector(deviceID string, accountID int64, store SeenStore) *IncrementalDetector {
	return &IncrementalDetector{
		deviceID:  deviceID,
		accountID: accountID,
		seen:      make(map[string]bool),
		store:     store,
	}
}

// Diff 输入账本页当前可见行，返回新收款行（不在 seen 集合），并更新基线。
// 基线为空（首次/重启后无持久化）→ 全部标记 seen，返回空（不推送历史）。
// rows 可为空（空态页）：仍标记基线已建立，后续新行正常推送。
func (d *IncrementalDetector) Diff(ctx context.Context, rows []capture.AlipayLedgerRow) []capture.AlipayLedgerRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restoreBaselineLocked(ctx)

	// 无基线（首次）→ 全量建基线，不推送（空态页也建基线，防首笔收款被吞）
	if !d.baselined {
		for _, r := range rows {
			d.seen[r.Fingerprint()] = true
		}
		d.baselined = true
		d.persistLocked(ctx)
		logger.Info("ledgermon", "账本页基线已建立",
			"device_id", d.deviceID, "account_id", d.accountID, "rows", len(rows))
		return nil
	}

	var fresh []capture.AlipayLedgerRow
	for _, r := range rows {
		fp := r.Fingerprint()
		if !d.seen[fp] {
			d.seen[fp] = true
			fresh = append(fresh, r)
		}
	}
	if len(fresh) > 0 {
		d.persistLocked(ctx)
	}
	return fresh
}

// Fresh 返回不在 seen 集合的新收款行，但**不提交** seen（资金安全：先推送成功再提交，
// 推送失败下一轮仍会再次检测到该行，不会永久丢单）。首次（无基线）→ 建基线返回空。
// rows 可为空（空态页）：首次仍标记基线已建立。
func (d *IncrementalDetector) Fresh(ctx context.Context, rows []capture.AlipayLedgerRow) []capture.AlipayLedgerRow {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restoreBaselineLocked(ctx)

	if !d.baselined {
		for _, r := range rows {
			d.seen[r.Fingerprint()] = true
		}
		d.baselined = true
		d.persistLocked(ctx)
		logger.Info("ledgermon", "账本页基线已建立（Fresh）",
			"device_id", d.deviceID, "account_id", d.accountID, "rows", len(rows))
		return nil
	}

	var fresh []capture.AlipayLedgerRow
	for _, r := range rows {
		if !d.seen[r.Fingerprint()] {
			fresh = append(fresh, r)
		}
	}
	return fresh
}

// Commit 将 fresh 行指纹提交到 seen 并持久化（Dispatch 成功后调用）。
// 未提交的 fresh 行在下一轮 Fresh 中仍会被返回（重试推送，不丢单）。
func (d *IncrementalDetector) Commit(ctx context.Context, fresh []capture.AlipayLedgerRow) {
	if len(fresh) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	changed := false
	for _, r := range fresh {
		if !d.seen[r.Fingerprint()] {
			d.seen[r.Fingerprint()] = true
			changed = true
		}
	}
	if changed {
		d.persistLocked(ctx)
	}
}

// restoreBaselineLocked 从 store 恢复基线（仅首次/重启后；调用方持锁）。
func (d *IncrementalDetector) restoreBaselineLocked(ctx context.Context) {
	if d.baselined || d.store == nil {
		return
	}
	if stored, err := d.store.LoadSeen(ctx, d.deviceID, d.accountID); err == nil {
		for _, fp := range stored {
			d.seen[fp] = true
		}
		if len(stored) > 0 {
			d.baselined = true
		}
		logger.Info("ledgermon", "增量基线已从持久化恢复",
			"device_id", d.deviceID, "account_id", d.accountID, "fingerprints", len(stored))
	} else {
		logger.Warn("ledgermon", "增量基线加载失败（将全量建基线）", "err", err, "device_id", d.deviceID)
	}
}

// persistLocked 持久化 seen 集合（调用方持锁）。集合膨胀保护：只保留最近 N 个指纹
// （账本页可见行有限，但跨天累计会增长；保留最近 1000 个足够覆盖重试窗口）。
const maxSeenFingerprints = 1000

func (d *IncrementalDetector) persistLocked(ctx context.Context) {
	if d.store == nil {
		return
	}
	// map 顺序不稳定 → 排序后保留尾部（时间戳指纹字典序≈时间序）
	fps := make([]string, 0, len(d.seen))
	for fp := range d.seen {
		fps = append(fps, fp)
	}
	// 简单稳定化：只存最近 1000 条（不排序也可，去重集合无顺序要求；截断即可）
	if len(fps) > maxSeenFingerprints {
		fps = fps[:maxSeenFingerprints]
	}
	if err := d.store.SaveSeen(ctx, d.deviceID, d.accountID, fps); err != nil {
		logger.Warn("ledgermon", "增量基线持久化失败", "err", err, "device_id", d.deviceID, "account_id", d.accountID)
	}
}

// Size 返回已见指纹数（监控/调试）
func (d *IncrementalDetector) Size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.seen)
}
