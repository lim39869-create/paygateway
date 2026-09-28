// Package reconciler 设备离线检测任务（个人免签通道）
//
// 设计（docs/MIANQIAN.md §9 掉线兜底与异常处理）：
//   - 设备离线判定以 adb 可达性为准（监听 App 已弃用，2026-08-29 重构）
//   - 设备 OFFLINE → 其绑定的收款账户冻结（不可接新单）
//   - 该账户下已下单未支付的订单 → 进人工异常池（listen_offline），
//     人工从收款记录核对实际收款情况（用户可能已付款但未监听到）
//
// 与 ConfirmScheduler 的 passive 检查互补：
//   - ConfirmScheduler.confirmPersonal 在关单时被动检查监听端是否离线
//   - 本任务主动扫描：设备一掉线立即冻结账户 + 挂起订单，不等关单时间
package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/adbmgr"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/order"
	"paygateway/internal/personal"
	"paygateway/internal/store"
)

// AdbProber 检测 adb 是否可达的接口（adbmgr.Manager 已实现此语义；抽象为接口便于 mock 与可选依赖）
type AdbProber interface {
	Online(ctx context.Context, serial string) bool
	Connect(ctx context.Context, serial string) error
}

// DeviceWatcherConfig 设备离线检测参数
type DeviceWatcherConfig struct {
	HeartbeatTimeout time.Duration // 心跳超时（默认 120s）
	ScanInterval     time.Duration // 扫描间隔（默认 30s）
	// MissThreshold 连续扫描未收到心跳的次数阈值，达到才判离线（默认 2）。
	// 配合 HeartbeatTimeout 形成「超时 + 连续确认」双保险：单次扫描抖动不误判。
	MissThreshold int
	// ControlPlaneInterval 控制面状态机扫描间隔（默认 30s）。
	// 独立于 ScanInterval：心跳超时与 adb 重连关注点不同，避免耦合。
	ControlPlaneInterval time.Duration
}

// DefaultDeviceWatcherConfig 默认参数
func DefaultDeviceWatcherConfig() *DeviceWatcherConfig {
	return &DeviceWatcherConfig{
		HeartbeatTimeout:     120 * time.Second,
		ScanInterval:         30 * time.Second,
		MissThreshold:        2,
		ControlPlaneInterval: 30 * time.Second,
	}
}

// Recoverer 设备离线恢复器（由 admin 包实现并注入），解耦 reconcile → admin 反向依赖。
// 实现侧负责限频，避免短时间对同一设备反复重启。
type Recoverer interface {
	AttemptRestart(ctx context.Context, id int64) error
}

// DeviceWatcher 监听端设备离线检测器
type DeviceWatcher struct {
	pdb       store.PersonalStore
	pool      *account.AccountPool
	st        store.Store
	svc       *order.Service
	cfg       *DeviceWatcherConfig
	recoverer Recoverer // nil = 关闭自动重启
	adb       AdbProber // nil = 关闭控制面状态机

	// missCount 记录每设备连续超时扫描次数（按设备 ID），达到 MissThreshold 才判离线
	missCount map[int64]int
	// controlPlaneLast 记录每设备上次控制面扫描时间（按 device_id），限频
	controlPlaneLast map[string]time.Time
}

// NewDeviceWatcher 创建设备离线检测器
// recoverer 可为 nil（关闭自动重启能力，行为退化为原始的「只冻结+挂单」语义）。
// adb 可为 nil（关闭控制面状态机，行为退化为「仅心跳冻结+挂单」语义）。
func NewDeviceWatcher(pdb store.PersonalStore, pool *account.AccountPool, st store.Store, svc *order.Service, cfg *DeviceWatcherConfig, recoverer Recoverer, adb AdbProber) *DeviceWatcher {
	if cfg == nil {
		cfg = DefaultDeviceWatcherConfig()
	}
	if cfg.HeartbeatTimeout <= 0 {
		cfg.HeartbeatTimeout = 120 * time.Second
	}
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = 30 * time.Second
	}
	if cfg.MissThreshold <= 0 {
		cfg.MissThreshold = 2
	}
	if cfg.ControlPlaneInterval <= 0 {
		cfg.ControlPlaneInterval = 30 * time.Second
	}
	return &DeviceWatcher{
		pdb: pdb, pool: pool, st: st, svc: svc, cfg: cfg, recoverer: recoverer,
		adb:              adb,
		missCount:        make(map[int64]int),
		controlPlaneLast: make(map[string]time.Time),
	}
}

// AsAdbProber 把 *adbmgr.Manager 适配为 AdbProber 接口（避免在 reconciler 包直接 import adbmgr 类型）。
func AsAdbProber(m *adbmgr.Manager) AdbProber {
	if m == nil {
		return nil
	}
	return adbProberAdapter{m}
}

type adbProberAdapter struct{ m *adbmgr.Manager }

func (a adbProberAdapter) Online(ctx context.Context, serial string) bool {
	return a.m.Online(ctx, serial)
}
func (a adbProberAdapter) Connect(ctx context.Context, serial string) error {
	return a.m.Connect(ctx, serial)
}

// Start 启动（阻塞，放 goroutine）
func (w *DeviceWatcher) Start(ctx context.Context) {
	logger.Info("reconciler", "监听端离线检测已启动",
		"heartbeat_timeout", w.cfg.HeartbeatTimeout.String(),
		"scan_interval", w.cfg.ScanInterval.String())
	ticker := time.NewTicker(w.cfg.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("reconciler", "监听端离线检测停止")
			return
		case <-ticker.C:
			w.scan(ctx)
			// 控制面状态机：adb prober 配置时才跑
			if w.adb != nil {
				w.scanControlPlane(ctx)
			}
		}
	}
}

// scan 单次扫描：以 adb 可达性为准维护设备在线状态（2026-08-29 重构：脱离监听 App 依赖）：
//   - adb 可达（USB/WiFi/LAN，含自动 adb connect 重连）→ 视为在线；
//     若此前离线（offline）则恢复 online + 解冻离线联动冻结的账户
//   - adb 不可达 → 回退心跳超时判定，超时判离线并冻结账户
func (w *DeviceWatcher) scan(ctx context.Context) {
	devs, err := w.pdb.ListDevices(ctx)
	if err != nil {
		logger.Error("reconciler", "设备列表查询失败", "err", err)
		return
	}
	for _, d := range devs {
		// 手动冻结/风控冻结的设备：不自动处理（仅人工解冻）
		if d.Status == string(personal.DeviceFrozen) {
			delete(w.missCount, d.ID)
			continue
		}
		// adb 可达优先：设备 serial 可连（自动 adb connect）→ 视为在线
		if w.adb != nil {
			if serial := adbSerialOf(d); serial != "" && w.adb.Online(ctx, serial) {
				if d.Status != string(personal.DeviceOnline) {
					w.recoverOnline(ctx, d) // 离线→在线：置 online + 解冻账户
				}
				delete(w.missCount, d.ID)
				continue
			}
		}
		// adb 不可达（且 status=online）：连续 miss 达阈值判离线 + 冻结联动。
		// 2026-08-30 重构：监听端 APP 弃用，LastHeartbeat 不再更新——此分支语义从
		// "心跳超时"收敛为"adb 不可达确认"（纯 adb 控制下 adb 是唯一存活信号），
		// 保留 missCount 防抖避免瞬时 adb 抖动误判离线。
		if d.Status != string(personal.DeviceOnline) {
			delete(w.missCount, d.ID)
			continue
		}
		w.countMiss(d.ID)
		if w.missCount[d.ID] >= w.cfg.MissThreshold {
			w.handleOffline(ctx, d, "adb 不可达（连续 "+strconv.Itoa(w.cfg.MissThreshold)+" 次扫描确认）")
			delete(w.missCount, d.ID)
		}
	}
}

// recoverOnline 设备 adb 恢复可达 → 置 online + 解冻离线联动冻结的绑定账户。
// 仅解冻 FrozenReason=offline 的账户；风控/手动冻结的账户保持 frozen（资金安全）。
func (w *DeviceWatcher) recoverOnline(ctx context.Context, d *personal.DeviceEncrypted) {
	logger.Info("reconciler", "设备 adb 恢复，置 online + 解冻离线联动账户", "device_id", d.DeviceID, "name", d.Name)
	if err := w.pdb.SetDeviceStatus(ctx, d.ID, string(personal.DeviceOnline)); err != nil {
		logger.Error("reconciler", "设备置在线失败", "err", err, "device_id", d.DeviceID)
	}
	for _, acctID := range store.DeviceAccountIDs(ctx, w.pdb, d.ID, d.AccountID) {
		if acct := w.pool.GetByID(acctID); acct != nil && acct.Status == account.StatusFrozen {
			w.pool.UnfreezeByOffline(acctID)
			if updated := w.pool.GetByID(acctID); updated != nil && updated.Status == account.StatusNormal {
				logger.Info("reconciler", "账户已自动解冻（设备 adb 恢复）", "account_id", acctID, "name", acct.Name)
			}
		}
	}
}

// adbSerialOf 解析设备 adb serial（TCP host:port 或 USB 纯 serial；与 cmd/gateway deviceADBSerial 同源）。
// 不可达/未配置返回空串（回退心跳判定）。
func adbSerialOf(d *personal.DeviceEncrypted) string {
	if d.AdbHost != "" {
		if d.AdbPort > 0 {
			return fmt.Sprintf("%s:%d", d.AdbHost, d.AdbPort)
		}
		return d.AdbHost
	}
	if d.AdbWifiHost != "" && d.AdbWifiPort > 0 {
		return fmt.Sprintf("%s:%d", d.AdbWifiHost, d.AdbWifiPort)
	}
	return ""
}

// countMiss 累加设备连续超时计数（防止 map 误写）
func (w *DeviceWatcher) countMiss(id int64) {
	w.missCount[id]++
}

// handleOffline 设备离线联动：置 OFFLINE → 冻结账户 → WAIT_PAY 订单进异常池
func (w *DeviceWatcher) handleOffline(ctx context.Context, d *personal.DeviceEncrypted, reason string) {
	logger.Warn("reconciler", "监听端离线", "device_id", d.DeviceID, "name", d.Name,
		"account_id", d.AccountID, "provider", d.Provider, "reason", reason)

	// 1. 设备置 OFFLINE
	if err := w.pdb.SetDeviceStatus(ctx, d.ID, string(personal.DeviceOffline)); err != nil {
		logger.Error("reconciler", "设备置离线失败", "err", err, "device_id", d.DeviceID)
	}

	// 1.5 触发后台自动重启尝试（不阻塞主流程；限频由 recoverer 实现侧负责）
	if w.recoverer != nil {
		go func(dev *personal.DeviceEncrypted) {
			rctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			if err := w.recoverer.AttemptRestart(rctx, dev.ID); err != nil {
				logger.Warn("reconciler", "设备自动重启尝试失败", "device_id", dev.DeviceID, "err", err)
			}
		}(d)
	}

	// 2. 冻结全部绑定账户（主账户 + 附加账户，不可接新单；设备 adb 恢复后自动解冻）
	for _, acctID := range store.DeviceAccountIDs(ctx, w.pdb, d.ID, d.AccountID) {
		acct := w.pool.GetByID(acctID)
		if acct != nil && acct.Status == account.StatusNormal {
			w.pool.FreezeByOffline(acctID)
			logger.Warn("reconciler", "账户已冻结（监听端离线联动）",
				"account_id", acctID, "name", acct.Name)
		}
		// 3. 该账户下 WAIT_PAY 订单进人工异常池（用户可能已付款但未监听到）
		w.enqueueWaitPayOrders(ctx, d, acctID)
	}
}

// enqueueWaitPayOrders 将指定账户下待支付订单挂起进异常池（幂等）
func (w *DeviceWatcher) enqueueWaitPayOrders(ctx context.Context, d *personal.DeviceEncrypted, accountID int64) {
	orders, err := w.st.ListWaitPayByAccount(ctx, accountID)
	if err != nil {
		logger.Error("reconciler", "查询待支付订单失败", "err", err, "account_id", accountID)
		return
	}
	for _, o := range orders {
		// 已进池的跳过（幂等）
		if existing, _ := w.st.GetExceptionByOutTradeNo(ctx, o.OutTradeNo); existing != nil {
			continue
		}
		ev := w.evidence(d, o)
		w.svc.EnqueueException(ctx, o, model.ExReasonListenOffline, "LISTEN_OFFLINE", "",
			ev, "监听端离线（"+d.DeviceID+"），无法确认用户是否已付款，需人工核实")
	}
}

// evidence 生成离线证据时间线片段
func (w *DeviceWatcher) evidence(d *personal.DeviceEncrypted, o *model.Order) string {
	m := map[string]any{
		"action":      "listen_offline",
		"time":        time.Now().Format(time.RFC3339),
		"device_id":   d.DeviceID,
		"device_name": d.Name,
		"provider":    d.Provider,
		"order":       o.OutTradeNo,
		"amount":      o.AmountMinor,
		"channel":     o.Channel,
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// scanControlPlane 控制面状态机扫描（docs/P2P-ADB-RECOVERY.md §6.3）：
// USB / 局域网 adb 可达 → 'usb'；都不可用 → 'tunnel_only'（adbd 监听依赖系统无线调试）。
//
// 触发时机：每 ControlPlaneInterval 跑一次（与心跳超时解耦；Phase 1 跟随 ScanInterval 共跑）。
func (w *DeviceWatcher) scanControlPlane(ctx context.Context) {
	devs, err := w.pdb.ListDevices(ctx)
	if err != nil {
		logger.Error("reconciler", "控制面扫描设备列表失败", "err", err)
		return
	}
	now := time.Now()
	for _, d := range devs {
		// 单设备限频：避免每轮都连同一条 adb 通道
		if last, ok := w.controlPlaneLast[d.DeviceID]; ok && now.Sub(last) < w.cfg.ControlPlaneInterval {
			continue
		}
		// 手动冻结的设备不处理控制面（evaluateControlPlane 内部仍会按 adb 可达性评估状态）
		if d.Status == string(personal.DeviceFrozen) {
			continue
		}
		w.controlPlaneLast[d.DeviceID] = now
		w.evaluateControlPlane(ctx, d)
	}
}

// evaluateControlPlane 单设备控制面状态计算（docs/P2P-ADB-RECOVERY.md §6.3）：
//  1. USB / 局域网 adb serial 在线 → 'usb'（最优先，本机通道延迟最低）
//  2. 不可达 → 'tunnel_only'（adbd 监听依赖系统无线调试）
//
// 监听 App 已弃用（2026-08-29）：relay 反向隧道分支移除，adb 可达性是唯一判定依据。
func (w *DeviceWatcher) evaluateControlPlane(ctx context.Context, d *personal.DeviceEncrypted) {
	// 1) USB / 局域网 adb 可达 → usb
	if w.adb != nil {
		if usbSerial := w.usbSerial(d); usbSerial != "" && w.adb.Online(ctx, usbSerial) {
			w.writeControlState(ctx, d, personal.ControlStateUSB)
			return
		}
	}
	// 2) 都不可用 → tunnel_only（adbd 监听依赖 Android 11+ 系统无线调试，App 侧无需也不应再下发复活指令）
	w.writeControlState(ctx, d, personal.ControlStateTunnelOnly)
}

// usbSerial 取设备首选 USB/WiFi 通道 serial（与 admin.personalHandler.deviceSerial 同语义）
func (w *DeviceWatcher) usbSerial(d *personal.DeviceEncrypted) string {
	if d.AdbHost != "" {
		if d.AdbPort > 0 {
			return d.AdbHost + ":" + strconv.Itoa(d.AdbPort)
		}
		return d.AdbHost
	}
	if d.AdbWifiHost != "" && d.AdbWifiPort > 0 {
		return d.AdbWifiHost + ":" + strconv.Itoa(d.AdbWifiPort)
	}
	return ""
}

// writeControlState 持久化控制面状态（状态未变不写库，避免噪声）
func (w *DeviceWatcher) writeControlState(ctx context.Context, d *personal.DeviceEncrypted, state string) {
	if d.ControlState == state {
		return // 状态未变 → 不写库，避免噪声
	}
	adbListen := d.AdbListen != 0
	// ts_ip 字段保留 DB 列（Phase 1 迁移产物，监听端暂时兼容可能携带该字段），
	// 但心跳路径不再写它 —— 用设备最新 DB 值兜底即可。
	if err := w.pdb.SetDeviceControlPlane(ctx, d.DeviceID, d.TsIP, adbListen, state); err != nil {
		logger.Warn("reconciler", "控制面状态写库失败", "err", err, "device_id", d.DeviceID, "state", state)
		return
	}
	d.ControlState = state
	logger.Info("reconciler", "控制面状态切换", "device_id", d.DeviceID, "name", d.Name, "state", state)
}
