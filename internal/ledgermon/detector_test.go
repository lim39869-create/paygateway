package ledgermon

import (
	"context"
	"testing"

	"paygateway/internal/admin/capture"
)

// memSeenStore 内存版 SeenStore（测试用，模拟 SQLite 持久化）
type memSeenStore struct {
	m map[string][]string // deviceID|accountID -> fingerprints
}

func newMemSeenStore() *memSeenStore { return &memSeenStore{m: map[string][]string{}} }

func (s *memSeenStore) LoadSeen(_ context.Context, deviceID string, accountID int64) ([]string, error) {
	return s.m[deviceID], nil
}
func (s *memSeenStore) SaveSeen(_ context.Context, deviceID string, accountID int64, fps []string) error {
	s.m[deviceID] = append([]string(nil), fps...)
	return nil
}

func row(time, amount string) capture.AlipayLedgerRow {
	return capture.AlipayLedgerRow{
		Payer:       "**怡",
		Time:        time,
		Amount:      amount,
		AmountMinor: 100,
	}
}

// TestDetector_FirstBaseline 首次读取（无基线）→ 全量建基线，不推送（防把历史收款当新收款）
func TestDetector_FirstBaseline(t *testing.T) {
	d := NewDetector("dev1", 1, nil)
	rows := []capture.AlipayLedgerRow{row("10:00:01", "+1.00"), row("10:00:02", "+2.00")}
	fresh := d.Diff(context.Background(), rows)
	if len(fresh) != 0 {
		t.Fatalf("首次应建基线不推送，实际推送 %d 行", len(fresh))
	}
	if d.Size() != 2 {
		t.Fatalf("基线应含 2 个指纹，实际 %d", d.Size())
	}
}

// TestDetector_Incremental 后续新增行 → 只推送新行
func TestDetector_Incremental(t *testing.T) {
	d := NewDetector("dev1", 1, nil)
	// 基线
	d.Diff(context.Background(), []capture.AlipayLedgerRow{row("10:00:01", "+1.00")})
	// 新增一行
	fresh := d.Diff(context.Background(), []capture.AlipayLedgerRow{
		row("10:00:01", "+1.00"), // 已见
		row("10:00:02", "+2.00"), // 新行
	})
	if len(fresh) != 1 || fresh[0].Time != "10:00:02" {
		t.Fatalf("应只推送新行 10:00:02，实际 %+v", fresh)
	}
}

// TestDetector_SameAmountMulti 同金额多笔：靠秒级时间戳区分，两笔都推送
func TestDetector_SameAmountMulti(t *testing.T) {
	d := NewDetector("dev1", 1, nil)
	d.Diff(context.Background(), []capture.AlipayLedgerRow{row("10:00:01", "+1.00")})
	fresh := d.Diff(context.Background(), []capture.AlipayLedgerRow{
		row("10:00:01", "+1.00"),
		row("10:00:02", "+1.00"), // 同金额不同秒
	})
	if len(fresh) != 1 || fresh[0].Time != "10:00:02" {
		t.Fatalf("同金额不同秒应作为新行推送，实际 %+v", fresh)
	}
}

// TestDetector_NoChange 页面无变化（重复读取）→ 不推送
func TestDetector_NoChange(t *testing.T) {
	d := NewDetector("dev1", 1, nil)
	rows := []capture.AlipayLedgerRow{row("10:00:01", "+1.00")}
	d.Diff(context.Background(), rows)
	if fresh := d.Diff(context.Background(), rows); len(fresh) != 0 {
		t.Fatalf("重复行不应推送，实际 %d 行", len(fresh))
	}
}

// TestDetector_RestartRestore 服务端重启：从 store 恢复基线，重启期间的新收款不丢
func TestDetector_RestartRestore(t *testing.T) {
	store := newMemSeenStore()
	// 第一次运行：建基线（10:00:01 已见）+ 新收款 10:00:02
	d1 := NewDetector("dev1", 1, store)
	d1.Diff(context.Background(), []capture.AlipayLedgerRow{row("10:00:01", "+1.00")})
	d1.Diff(context.Background(), []capture.AlipayLedgerRow{
		row("10:00:01", "+1.00"), row("10:00:02", "+2.00"),
	})
	// 模拟重启：新 detector 从 store 恢复
	d2 := NewDetector("dev1", 1, store)
	// 重启期间新收款 10:00:03 已出现在账本页
	fresh := d2.Diff(context.Background(), []capture.AlipayLedgerRow{
		row("10:00:01", "+1.00"), // 历史已见 → 不推
		row("10:00:02", "+2.00"), // 重启前已处理 → 不推
		row("10:00:03", "+3.00"), // 重启期间新收款 → 推
	})
	if len(fresh) != 1 || fresh[0].Time != "10:00:03" {
		t.Fatalf("重启后应只推送新收款 10:00:03，实际 %+v", fresh)
	}
}

// TestDetector_RestartWithoutStore 重启但无持久化 → 重新全量建基线（不推送历史）
func TestDetector_RestartWithoutStore(t *testing.T) {
	d1 := NewDetector("dev1", 1, nil)
	d1.Diff(context.Background(), []capture.AlipayLedgerRow{row("10:00:01", "+1.00")})
	// 重启（无 store）：全部行视为历史基线
	d2 := NewDetector("dev1", 1, nil)
	fresh := d2.Diff(context.Background(), []capture.AlipayLedgerRow{
		row("10:00:01", "+1.00"), row("10:00:02", "+2.00"),
	})
	if len(fresh) != 0 {
		t.Fatalf("无持久化重启应全量建基线不推送，实际 %d 行", len(fresh))
	}
}

// TestDetector_PersistAfterBaseline 基线建立后应持久化（store 有数据）
func TestDetector_PersistAfterBaseline(t *testing.T) {
	store := newMemSeenStore()
	d := NewDetector("dev1", 1, store)
	d.Diff(context.Background(), []capture.AlipayLedgerRow{row("10:00:01", "+1.00")})
	if stored := store.m["dev1"]; len(stored) != 1 {
		t.Fatalf("基线应持久化 1 个指纹，实际 %d", len(stored))
	}
}
