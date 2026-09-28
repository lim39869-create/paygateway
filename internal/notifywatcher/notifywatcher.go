// Package notifywatcher 通知栏快速通道监听（独立于 ledgermon 的账本慢通道）。
//
// 背景（2026-08-30 真机实测 + 用户需求确认）：
//   - 微信/支付宝收款到账会弹系统通知（dumpsys notification --noredact 可读全文）；
//   - 通知栏比账本页快 ~15s，是收款感知的快速通道；
//   - 通知栏数据不清理会一直存在 → 匹配窗口必须以通知 when（收款发生时刻）为锚，
//     不能用 time.Now()（否则旧通知会错误匹配新订单）；
//   - 账户区分靠 uid：微信/支付宝主空间 uid 无 999 前缀，双开(XSpace) uid 有 999 前缀
//     （实测：com.tencent.mm uid=10277 主号 / uid=99910277 双开；支付宝同理 10250/99910250）。
//
// 本包只负责：设备枚举 → 按设备轮询 dumpsys notification --noredact → 解析 →
// 指纹去重 → 回调 main.go 注入的匹配入账函数（复用 matchPersonalOrder 链路）。
// 不直接触碰 UI/账本页，与 ledgermon 的 DeviceActor 串行队列零竞争。
package notifywatcher

import (
	"context"
	"sync"
	"time"

	"paygateway/internal/logger"
)

// 默认轮询参数（Config 未设置时兜底）。
const (
	defaultPollEvery    = 500 * time.Millisecond // 有 WAIT_PAY 订单时的高频轮询
	defaultIdleEvery    = 5 * time.Second        // 池空时的低频探测
	defaultShellTimeout = 3 * time.Second        // 单次 dumpsys 超时
)

// Device 设备级监听单元（由 DeviceResolver 枚举，main.go 注入）。
type Device struct {
	DeviceID string // personal.devices.device_id
	Serial   string // adb serial（host:port 或 USB 序列号）
}

// DeviceResolver 在线设备枚举（main.go 注入：ListDevices + adb.Online 过滤）。
type DeviceResolver func(ctx context.Context) ([]Device, error)

// HasPending 是否仍有待支付订单（main.go 注入：waitPayPool.Snapshot().Total > 0）。
// 池 reconcile 间隙可能短暂为空，调用方用低频探测兜底。
type HasPending func(ctx context.Context) bool

// Handler 收款通知回调（main.go 注入：复用 matchPersonalOrder 入账链路）。
// 返回 error 表示处理失败（记录日志，不影响轮询继续）。
type Handler func(ctx context.Context, ev Event) error

// Sheller adb 命令执行抽象（生产 = *adbmgr.Manager；测试 = fake 桩）。
// 只暴露 dumpsys 所需的 Shell 方法，与 ledgermon 的 UI 操作零交集。
type Sheller interface {
	Shell(ctx context.Context, timeout time.Duration, serial, command string) (string, error)
}

// SeenStore 去重存储（main.go 注入：personalDB.PushExists）。
// 指纹含设备ID+通知key+金额+when，同一通知跨轮幂等。
type SeenStore interface {
	PushExists(ctx context.Context, deviceID, rawHash string) (bool, error)
}

// Config 轮询参数（零值使用默认）。
type Config struct {
	PollEvery    time.Duration // 有 WAIT_PAY 订单轮询间隔（默认 500ms）
	IdleEvery    time.Duration // 池空探测间隔（默认 5s）
	ShellTimeout time.Duration // 单次 dumpsys 超时（默认 3s）
}

// Manager 通知栏快速通道监听器。
//
// 与 ledgermon 的关系（架构决策，用户确认）：
//   - 完全独立：自己的 goroutine + 自己的 adbMgr() 实例（Manager.mu 独立 → 天然并行，
//     实测 dumpsys notification ∥ uiautomator dump ∥ input 互不阻塞）；
//   - 订单驱动：waitPayPool 有订单 → 高频轮询；池空 → 低频探测，不空转；
//   - 匹配回调复用 main.go 的 matchPersonalOrder（空间账户过滤 + 金额时间窗 + 冻结守卫），
//     与账本链路共享同一套资金安全逻辑，绝不另起炉灶。
type Manager struct {
	adb       Sheller
	resolve   DeviceResolver
	hasPendFn HasPending // 订单驱动判定（nil 视为恒有空闲）
	handler   Handler
	seen      SeenStore
	cfg       Config
	wake      chan struct{} // 事件唤醒信号（新订单进池触发，容量1信号合并）

	mu       sync.Mutex
	lastPoll map[string]time.Time // deviceID → 上次轮询时刻（日志节流）
}

// New 创建通知栏监听器。adb 为 nil 时禁用（设备枚举阶段直接返回）。
func New(adb Sheller, resolve DeviceResolver, hasPend HasPending,
	handler Handler, seen SeenStore, cfg Config) *Manager {
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = defaultPollEvery
	}
	if cfg.IdleEvery <= 0 {
		cfg.IdleEvery = defaultIdleEvery
	}
	if cfg.ShellTimeout <= 0 {
		cfg.ShellTimeout = defaultShellTimeout
	}
	return &Manager{
		adb: adb, resolve: resolve, hasPendFn: hasPend, handler: handler,
		seen: seen, cfg: cfg, lastPoll: make(map[string]time.Time),
		wake: make(chan struct{}, 1),
	}
}

// Wake 事件唤醒：新订单进入 WAIT_PAY 池时调用（main.go 经 SetWaitPayHooks.Add 链入）。
// 池空休眠态立即醒来轮询一轮；轮询态发送不阻塞（信号合并）。
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default: // 已有待消费信号，合并
	}
}

// Start 启动监听循环（阻塞，调用方 go 启动）。
//
// 事件驱动（2026-08-30 用户确认）：池空 → 完全停止轮询（零空转），阻塞等待
// Wake 事件或低频兜底定时器；新订单进池（Wake）→ 立即轮询，无间隔延迟。
// idle 兜底：应对池快照与 DB 短暂不一致（reconcile 间隙），防漏唤醒长期休眠。
func (m *Manager) Start(ctx context.Context) {
	if m.adb == nil || m.resolve == nil || m.handler == nil {
		logger.Warn("notifywatcher", "依赖缺失，通知栏监听未启动",
			"adb", m.adb != nil, "resolve", m.resolve != nil, "handler", m.handler != nil)
		return
	}
	logger.Info("notifywatcher", "通知栏快速通道监听已启动（事件驱动）",
		"poll", m.cfg.PollEvery.String(), "idle", m.cfg.IdleEvery.String())

	for {
		// 订单驱动：池有订单才轮询；池空 → 阻塞等事件/兜底定时器
		if m.hasPendFn != nil && m.hasPendFn(ctx) {
			m.pollOnce(ctx)
			select {
			case <-ctx.Done():
				logger.Info("notifywatcher", "通知栏监听已停止")
				return
			case <-m.wake:
				// 唤醒信号：直接继续下一轮（高频路径）
			case <-time.After(m.cfg.PollEvery):
			}
			continue
		}
		// 池空休眠：等新订单事件（即时唤醒）或低频兜底探测（防 reconcile 间隙漏唤醒）
		select {
		case <-ctx.Done():
			logger.Info("notifywatcher", "通知栏监听已停止")
			return
		case <-m.wake:
		case <-time.After(m.cfg.IdleEvery):
		}
	}
}

// pollOnce 单轮：枚举设备 → 逐台 dump 通知栏 → 解析 → 去重 → 回调。
func (m *Manager) pollOnce(ctx context.Context) {
	devs, err := m.resolve(ctx)
	if err != nil {
		logger.Warn("notifywatcher", "设备枚举失败（下轮重试）", "err", err)
		return
	}
	for _, d := range devs {
		if err := m.pollDevice(ctx, d); err != nil {
			// 单设备失败不中断其他设备
			logger.Warn("notifywatcher", "设备通知轮询失败",
				"err", err, "device_id", d.DeviceID, "serial", d.Serial)
		}
	}
}

// pollDevice 单设备：dump → 解析 → 逐条去重回调。
func (m *Manager) pollDevice(ctx context.Context, d Device) error {
	if d.Serial == "" {
		return nil
	}
	out, err := m.adb.Shell(ctx, m.cfg.ShellTimeout, d.Serial, "dumpsys notification --noredact")
	if err != nil {
		return err
	}
	evs := Parse(out, d.DeviceID)
	if len(evs) == 0 {
		return nil
	}
	for i := range evs {
		ev := evs[i]
		// 指纹去重：同设备同通知只处理一次（通知栏不清理会一直在）
		fp := ev.Fingerprint()
		if m.seen != nil {
			dup, derr := m.seen.PushExists(ctx, d.DeviceID, fp)
			if derr != nil {
				logger.Error("notifywatcher", "去重查询失败（按未处理继续）",
					"err", derr, "device_id", d.DeviceID)
			} else if dup {
				continue
			}
		}
		// 节流日志：同设备每秒最多一条（防止池空时高频刷屏）
		m.throttledLog(d.DeviceID, ev)
		if err := m.handler(ctx, ev); err != nil {
			logger.Error("notifywatcher", "收款通知处理失败",
				"err", err, "device_id", d.DeviceID,
				"provider", ev.Provider, "space", ev.Space, "amount", ev.AmountMinor)
			continue
		}
	}
	return nil
}

// throttledLog 通知捕获日志（每设备 1s 节流）。
func (m *Manager) throttledLog(deviceID string, ev Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if last, ok := m.lastPoll[deviceID]; ok && now.Sub(last) < time.Second {
		return
	}
	m.lastPoll[deviceID] = now
	logger.Info("notifywatcher", "捕获收款通知",
		"device_id", deviceID, "provider", ev.Provider,
		"space", ev.Space, "amount_minor", ev.AmountMinor, "when", ev.When)
}
