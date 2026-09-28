package ledgermon

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"paygateway/internal/adbmgr"
	"paygateway/internal/admin/capture"
	"paygateway/internal/logger"
	"paygateway/internal/pool"
)

// DispatchFunc 收款事件推送回调（由 main.go 注入：复用匹配+入账链路，见 docs §6.1）
// 行无备注，匹配走金额+时间窗口；source=ledger。space 为账本页所在用户空间（0=主，999=双开），
// 透传给匹配链路区分双开账户收款。
type DispatchFunc func(ctx context.Context, deviceID string, provider string, space int, rows []capture.AlipayLedgerRow) error

// LedgerMonitor 每账户命令生产者（DeviceActor 模型，docs/DESIGN-DEVICE-ACTOR.md §4.2）
//
// 从"拥有 goroutine 的执行者"降级为**纯命令生产者**：保留账户级状态机
// （订单驱动暂停/激活/终结判定），所有 adb 操作通过 DeviceActor 命令队列执行，
// 由 actor 串行化（每设备单执行者），天然互斥、无抢占。
type LedgerMonitor struct {
	accountID int64
	deviceID  string
	provider  string
	serial    string
	userId    int // 0=主空间，999=双开分身
	// actor 设备执行者（投递命令用，reconcile 创建 monitor 时注入）
	actor *DeviceActor
	// hasPending 该账户是否仍有待支付订单（main.go 注入：ListWaitPayByAccount 非空）
	hasPending func(ctx context.Context, accountID int64) bool
	pollEvery  time.Duration // 有待支付订单时的轮询间隔（默认 1s）
	wake       chan struct{} // 下单激活信号（非阻塞发送）
	stop       chan struct{}
	done       chan struct{}
	// monitorActive 当前是否处于轮询态（admin 状态展示）
	monitorActive atomic.Bool
	// warmupDone 预热完成信号（close 后 Manager 可判断设备所有账户已就绪）
	warmupDone chan struct{}
	// schedulerMode 由 DeviceScheduler 接管轮询（公平调度）；本 monitor 只做预热。
	// 订单驱动轮询语义移交给调度器，monitor 完成预热后等待 stop（不再投 PollCmd）。
	schedulerMode bool
}

// NewMonitor 创建账户命令生产者。pollEvery<=0 用默认 1s。
func NewMonitor(accountID int64, deviceID, provider, serial string, userId int,
	actor *DeviceActor, hasPending func(ctx context.Context, accountID int64) bool,
	pollEvery time.Duration) *LedgerMonitor {
	if pollEvery <= 0 {
		pollEvery = 1 * time.Second
	}
	return &LedgerMonitor{
		accountID: accountID, deviceID: deviceID, provider: provider, serial: serial, userId: userId,
		actor: actor, hasPending: hasPending, pollEvery: pollEvery,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
		warmupDone: make(chan struct{}),
	}
}

// Start 启动命令生产者（后台 goroutine，初始暂停态）
func (m *LedgerMonitor) Start() { go m.loop() }

// Stop 停止命令生产者（等待退出）
func (m *LedgerMonitor) Stop() {
	close(m.stop)
	<-m.done
}

// SetSchedulerMode 启用公平调度模式：monitor 只做预热，轮询由 DeviceScheduler 接管。
// 必须在 Start() 之前调用。
func (m *LedgerMonitor) SetSchedulerMode() { m.schedulerMode = true }

// Activate 下单钩子：唤醒命令生产者（非阻塞；已在轮询则幂等）。
// 唤醒后向 actor 周期投递 PollCmd，轮询持续到该账户待支付订单终结。
// 公平调度模式下唤醒被忽略（轮询由 DeviceScheduler 统一决策）。
func (m *LedgerMonitor) Activate() {
	if m.schedulerMode {
		return
	}
	select {
	case m.wake <- struct{}{}:
	default: // 已在唤醒队列/轮询中
	}
}

// IsActive 是否处于轮询态（admin 状态展示）
func (m *LedgerMonitor) IsActive() bool { return m.monitorActive.Load() }

// loop 订单驱动命令生产循环：预热 → 等待唤醒 → 有待支付订单才周期投递 PollCmd →
// 订单终结暂停。命令只入队不执行——执行在 DeviceActor 内串行完成（天然互斥）。
func (m *LedgerMonitor) loop() {
	defer close(m.done)
	ctx := context.Background()
	// 预热：投递 WarmupCmd 等待完成（设备上线即导航建基线，不依赖订单）。
	// actor 串行执行，多账户预热天然排队，无互相抢占。
	m.warmupOnce(ctx)
	// 公平调度模式：轮询由 DeviceScheduler 统一决策（读内存池快照），
	// monitor 预热完成后仅响应 stop（不投 PollCmd，避免双生产者）。
	if m.schedulerMode {
		<-m.stop
		return
	}
	for {
		// 暂停态：等待下单唤醒
		select {
		case <-m.stop:
			return
		case <-m.wake:
		}
		// 唤醒后确认有待支付订单；无（竞态：下单后立即终结）→ 直接回暂停
		if !m.hasPendingOrder(ctx) {
			continue
		}
		// 轮询态：每次 poll 后检查订单，终结立即暂停（不空转）
		m.monitorActive.Store(true)
		for {
			select {
			case <-m.stop:
				return
			case <-time.After(m.pollEvery):
			}
			m.pollOnce(ctx)
			if !m.hasPendingOrder(ctx) {
				m.monitorActive.Store(false)
				logger.Info("ledgermon", "账户待支付订单已终结，监听暂停",
					"account_id", m.accountID, "device_id", m.deviceID)
				break
			}
		}
	}
}

// warmupOnce 投递预热命令并等待完成。
// 预热失败重试（带退避）：设备上线预热建基线是必要动作，单次偶发失败（如
// uiautomator dump 冷启动被 kill —— 真机实测 account3 一次 `signal: killed`
// 后整会话无基线，导致后续订单查询全失败）不应永久搁浅。此处是设备上线
// 无实时回调的安全位置，冷启动重试不会干扰订单支付确认。
// 预热命令在 actor 队列里与轮询/手动预热串行，天然无竞态。
func (m *LedgerMonitor) warmupOnce(ctx context.Context) {
	const maxRetries = 3
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		done := make(chan error, 1)
		m.actor.Enqueue(&WarmupCmd{
			AccountID: m.accountID,
			Provider:  m.provider,
			UserID:    m.userId,
			Done:      done,
		})
		lastErr = <-done // 等 actor 执行完
		if lastErr == nil {
			break
		}
		if attempt < maxRetries {
			logger.Warn("ledgermon", "预热失败，即将重试",
				"account_id", m.accountID, "provider", m.provider,
				"attempt", attempt+1, "err", lastErr)
			select {
			case <-ctx.Done():
				return
			case <-time.After(warmupRetryDelay):
			}
		}
	}
	if lastErr != nil {
		logger.Warn("ledgermon", "预热多次重试后仍失败",
			"account_id", m.accountID, "provider", m.provider, "err", lastErr)
	}
	m.signalWarmupDone()
}

// pollOnce 投递单轮轮询命令（不等待执行——actor 串行队列保证不重叠）。
func (m *LedgerMonitor) pollOnce(ctx context.Context) {
	m.actor.Enqueue(&PollCmd{AccountID: m.accountID, Provider: m.provider, UserID: m.userId})
}

// hasPendingOrder 该账户是否仍有待支付订单（未注入时视为有，保持轮询）
func (m *LedgerMonitor) hasPendingOrder(ctx context.Context) bool {
	if m.hasPending == nil {
		return true
	}
	return m.hasPending(ctx, m.accountID)
}

// signalWarmupDone 标记预热完成（幂等：close 一次后再次调用无副作用）。
func (m *LedgerMonitor) signalWarmupDone() {
	select {
	case <-m.warmupDone:
	default:
		close(m.warmupDone)
	}
}

// WarmupDone 返回预热完成信号（Manager 设备级编排用：全部账户就绪后统一锁定任务）
func (m *LedgerMonitor) WarmupDone() <-chan struct{} { return m.warmupDone }

// ========== 编排管理器 ==========

// MonitoredDevice 一台待监听设备（由 main.go 解析注入：设备 + 可服务账户 + adb serial）
type MonitoredDevice struct {
	DeviceID  string
	Serial    string
	Provider  string
	AccountID int64
	UserID    int // 0=主空间，999=双开
}

// DeviceResolver 设备解析回调（main.go 注入：从 personal.db 枚举在线设备 + 账户 + serial）
type DeviceResolver func(ctx context.Context) ([]MonitoredDevice, error)

// ReaderFactory 按设备构造读取器（main.go 注入：封装 adb 管理器 + 通道分派）
type ReaderFactory func(d MonitoredDevice) LedgerReader

// Manager 监听编排（DeviceActor 模型）：按设备解析账户 → 创建 DeviceActor +
// 命令生产者（公平调度模式：DeviceScheduler；旧模式：LedgerMonitor）→ 状态查询。
// 每设备一个 actor，串行执行全部 adb 操作。
type Manager struct {
	mu         sync.Mutex
	actors     map[string]*DeviceActor     // key: deviceID
	monitors   map[string]*LedgerMonitor   // key: monitorKey(deviceID, provider, userID)
	scheds     map[string]*DeviceScheduler // key: deviceID（公平调度模式；pool 非 nil 时启用）
	resolver   DeviceResolver
	factory    ReaderFactory
	dispatch   DispatchFunc
	adb        *adbmgr.Manager
	hasPending func(ctx context.Context, accountID int64) bool
	pollEvery  time.Duration
	store      SeenStore
	// pool WAIT_PAY 内存订单池（非 nil → 公平调度模式：DeviceScheduler 接管轮询）
	pool *pool.WaitPayPool
	// lockArmed 每设备锁定协调是否已启动（map[deviceID]struct{}，防重复启动）
	lockArmed map[string]struct{}
	// lockDone 每设备锁定是否已完成（map[deviceID]struct{}，防重复执行）
	lockDone map[string]struct{}
	// warmupInFlight 每设备手动预热是否进行中（map[deviceID]bool，防重复点击堆积）
	warmupInFlight map[string]bool
}

// monitorKey 账户命令生产者唯一键：(deviceID, provider, userID)。
// 同一设备的账本页实例 = 每 (平台, 用户空间) 一个（支付宝主空间/双开、微信主空间/双开各不同），
// 必须含 provider 维度——否则 alipay@user0 与 wechat@user0 会碰撞覆盖。
func monitorKey(deviceID, provider string, userID int) string {
	return fmt.Sprintf("%s|%s|%d", deviceID, provider, userID)
}

// NewManager 创建编排器。store 可 nil（增量仅内存）。
// hasPending 判断账户是否仍有待支付订单（main.go 注入；nil = 视为恒有，仅测试用）。
// adb 用于 ManualWarmupCmd/LockCmd 直连 capture（可 nil：不启用设备级手动预热/锁定）。
// waitPayPool 非 nil → 公平调度模式：DeviceScheduler 按内存池快照统一决策轮询（推荐）；
// nil → 旧模式：LedgerMonitor 订单驱动轮询（测试/无池环境兼容）。
func NewManager(adb *adbmgr.Manager, resolver DeviceResolver, factory ReaderFactory,
	dispatch DispatchFunc, hasPending func(ctx context.Context, accountID int64) bool,
	pollEvery time.Duration, store SeenStore, waitPayPool *pool.WaitPayPool) *Manager {
	if pollEvery <= 0 {
		pollEvery = 1 * time.Second
	}
	return &Manager{
		adb: adb, resolver: resolver, factory: factory, dispatch: dispatch,
		hasPending: hasPending, pollEvery: pollEvery, store: store,
		pool:           waitPayPool,
		actors:         make(map[string]*DeviceActor),
		monitors:       make(map[string]*LedgerMonitor),
		scheds:         make(map[string]*DeviceScheduler),
		lockArmed:      make(map[string]struct{}),
		lockDone:       make(map[string]struct{}),
		warmupInFlight: make(map[string]bool),
	}
}

// Start 启动编排：周期 reconcile（设备增删/下线自动收敛）。
func (m *Manager) Start(ctx context.Context) {
	go m.reconcileLoop(ctx)
}

// reconcileLoop 周期扫描设备（30s）：新增设备创建 actor/生产者，消失设备停止
func (m *Manager) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	m.reconcileOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce 按设备收敛：创建设备 DeviceActor（幂等），为每个账户创建命令生产者，
// 并向 actor 投递 WarmupCmd。设备消失 → 停止 actor 与全部生产者。
func (m *Manager) reconcileOnce(ctx context.Context) {
	if m.resolver == nil {
		return
	}
	devs, err := m.resolver(ctx)
	if err != nil {
		logger.Warn("ledgermon", "设备解析失败", "err", err)
		return
	}
	seen := make(map[string]bool)
	m.mu.Lock()
	defer m.mu.Unlock()

	// 按设备分组（消除 resolver 返回顺序依赖：任一账户有 reader 即建 actor，其余逐账户补齐）
	byDevice := make(map[string][]MonitoredDevice)
	for _, d := range devs {
		byDevice[d.DeviceID] = append(byDevice[d.DeviceID], d)
	}
	for deviceID, list := range byDevice {
		for _, d := range list {
			seen[monitorKey(d.DeviceID, d.Provider, d.UserID)] = true
		}
		// 设备级 actor：每设备恰好一个（幂等创建）。任一账户有读取通道即建，
		// 不再依赖"首个被枚举账户"的 provider 是否实现。
		actor := m.actors[deviceID]
		if actor == nil {
			var reader LedgerReader
			for _, d := range list {
				if m.factory == nil {
					break
				}
				if reader = m.factory(d); reader != nil {
					actor = NewDeviceActor(d.DeviceID, d.Serial, m.adb, m.dispatch)
					actor.AddAccount(&AccountCtx{
						AccountID: d.AccountID,
						Provider:  d.Provider,
						UserID:    d.UserID,
						Reader:    reader,
						Detector:  NewDetector(d.DeviceID, d.AccountID, m.store),
					})
					m.actors[d.DeviceID] = actor
					logger.Info("ledgermon", "设备 DeviceActor 已创建（串行执行者）",
						"device_id", d.DeviceID, "serial", d.Serial)
					break
				}
			}
			if actor == nil {
				logger.Info("ledgermon", "设备全部读取通道未实现，跳过监听",
					"device_id", deviceID, "accounts", len(list))
				continue
			}
		}
		// 账户级命令生产者：每 (账户, 通道, 空间) 一个（幂等创建）
		for _, d := range list {
			key := monitorKey(d.DeviceID, d.Provider, d.UserID)
			if _, ok := m.monitors[key]; ok {
				continue
			}
			// 为 actor 补齐该账户的读取上下文
			if acct := actor.DeviceCtx.Account(d.Provider, d.UserID); acct == nil {
				reader := m.factory(d)
				if reader == nil {
					continue
				}
				actor.AddAccount(&AccountCtx{
					AccountID: d.AccountID,
					Provider:  d.Provider,
					UserID:    d.UserID,
					Reader:    reader,
					Detector:  NewDetector(d.DeviceID, d.AccountID, m.store),
				})
			}
			mon := NewMonitor(d.AccountID, d.DeviceID, d.Provider, d.Serial, d.UserID,
				actor, m.hasPending, m.pollEvery)
			if m.pool != nil {
				// 公平调度模式：monitor 只做预热，轮询由 DeviceScheduler 统一决策
				mon.SetSchedulerMode()
			}
			m.monitors[key] = mon
			mon.Start()
			logger.Info("ledgermon", "账户命令生产者已启动（初始暂停，等待下单唤醒）",
				"device_id", d.DeviceID, "account_id", d.AccountID, "user_id", d.UserID, "provider", d.Provider)
			m.armDeviceLock(d.DeviceID)
		}
		// 公平调度模式：每设备一个 DeviceScheduler（幂等创建），账户快照同步给调度器
		if m.pool != nil {
			m.ensureScheduler(deviceID, actor)
		}
	}
	// 移除消失设备（先停账户生产者，再停设备 actor）
	for key, mon := range m.monitors {
		if !seen[key] {
			mon.Stop()
			delete(m.monitors, key)
			logger.Info("ledgermon", "账户命令生产者已停止（账户/设备消失）", "device_id", mon.deviceID)
		}
	}
	// 公平调度模式：设备消失 → 停止该设备 scheduler
	for deviceID, sched := range m.scheds {
		if _, ok := byDevice[deviceID]; !ok {
			sched.Stop()
			delete(m.scheds, deviceID)
			logger.Info("ledgermon", "设备 DeviceScheduler 已停止（设备消失）", "device_id", deviceID)
		}
	}
	for deviceID, actor := range m.actors {
		hasAccount := false
		for key := range m.monitors {
			if mon := m.monitors[key]; mon.deviceID == deviceID {
				hasAccount = true
				break
			}
		}
		if !hasAccount {
			actor.Stop()
			delete(m.actors, deviceID)
			delete(m.lockArmed, deviceID)
			delete(m.lockDone, deviceID)
			logger.Info("ledgermon", "设备 DeviceActor 已停止（设备消失）", "device_id", deviceID)
		}
	}
}

// ensureScheduler 公平调度模式：每设备幂等创建 DeviceScheduler，并同步账户快照。
// 账户快照从 actor 账户上下文提取（accountID → provider/userID），供调度器按池匹配。
// 调用方持 m.mu 锁。
func (m *Manager) ensureScheduler(deviceID string, actor *DeviceActor) {
	if m.pool == nil {
		return
	}
	sched := m.scheds[deviceID]
	if sched == nil {
		sched = NewDeviceScheduler(deviceID, actor, m.pool, DefaultSchedulerConfig())
		m.scheds[deviceID] = sched
		sched.Start()
		logger.Info("ledgermon", "设备 DeviceScheduler 已创建（公平时间占比调度）",
			"device_id", deviceID)
	}
	sched.SetAccounts(schedulerAcctsOf(actor))
}

// schedulerAcctsOf 从 actor 账户快照提取调度器视图（accountID → provider/userID）
func schedulerAcctsOf(actor *DeviceActor) []schedulerAcct {
	accts := actor.DeviceCtx.Accounts()
	out := make([]schedulerAcct, 0, len(accts))
	for _, ac := range accts {
		out = append(out, schedulerAcct{accountID: ac.AccountID, provider: ac.Provider, userID: ac.UserID})
	}
	return out
}

// armDeviceLock 为设备启动锁定协调（幂等：同设备只启动一次）。
// 等设备全部账户 WarmupCmd 完成后，向 actor 投递 LockCmd（锁定最近任务，防一键清理）。
// 锁定在 actor 队列内串行执行——天然不会与账户导航/轮询抢前台。
func (m *Manager) armDeviceLock(deviceID string) {
	if _, ok := m.lockDone[deviceID]; ok {
		return // 本设备已锁定过，跳过
	}
	if _, ok := m.lockArmed[deviceID]; ok {
		return // 协调已启动，等待即可
	}
	if m.adb == nil {
		return // 无 adb（测试环境），锁定无意义
	}
	m.lockArmed[deviceID] = struct{}{}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("ledgermon", "设备加锁协程 panic（已恢复）", "device_id", deviceID,
					"panic", r, "stack", string(debug.Stack()))
			}
		}()
		m.waitAllWarmup(deviceID)
		m.mu.Lock()
		actor := m.actors[deviceID]
		m.mu.Unlock()
		if actor != nil {
			actor.Enqueue(&LockCmd{})
		}
		m.mu.Lock()
		m.lockDone[deviceID] = struct{}{}
		delete(m.lockArmed, deviceID)
		m.mu.Unlock()
	}()
}

// waitAllWarmup 阻塞等待设备全部账户预热完成（任一账户预热失败 → 本轮不等待超时）。
func (m *Manager) waitAllWarmup(deviceID string) {
	deadline := time.Now().Add(3 * time.Minute) // 冷启动导航上限保护
	for time.Now().Before(deadline) {
		m.mu.Lock()
		var chans []<-chan struct{}
		for _, mon := range m.monitors {
			if mon.deviceID == deviceID {
				chans = append(chans, mon.WarmupDone())
			}
		}
		m.mu.Unlock()
		if len(chans) == 0 {
			time.Sleep(2 * time.Second)
			continue
		}
		allDone := true
		for _, ch := range chans {
			select {
			case <-ch:
			default:
				allDone = false
			}
		}
		if allDone {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// ActivateAccount 下单钩子（main.go 调用）：路由账户后唤醒对应命令生产者。
// 公平调度模式：唤醒所属设备 scheduler（池空休眠时立即响应新订单）；
// 旧模式：唤醒 LedgerMonitor。
func (m *Manager) ActivateAccount(accountID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mon := range m.monitors {
		if mon.accountID == accountID {
			if sched := m.scheds[mon.deviceID]; sched != nil {
				sched.Wake()
				logger.Info("ledgermon", "下单触发设备调度器唤醒",
					"account_id", accountID, "device_id", mon.deviceID)
				return
			}
			mon.Activate()
			logger.Info("ledgermon", "下单触发账户监听激活",
				"account_id", accountID, "device_id", mon.deviceID)
			return
		}
	}
	logger.Debug("ledgermon", "账户无监听循环，忽略下单激活", "account_id", accountID)
}

// WarmupDevice 手动预热（admin 触发）：向设备 actor 投递 ManualWarmupCmd 并等待完成。
// ManualWarmupCmd 是队列中的一个命令——与自动预热/轮询天然互斥，无特殊分支。
// providers 指定通道（{"alipay":true,"wechat":true}），空 = 全部。
// 每设备单飞：已有手动预热在排队/执行时直接返回错误，防重复点击堆积（每个最长 240s）。
func (m *Manager) WarmupDevice(ctx context.Context, deviceID string, providers map[string]bool) error {
	m.mu.Lock()
	actor := m.actors[deviceID]
	if actor == nil {
		m.mu.Unlock()
		return fmt.Errorf("设备无 DeviceActor（设备离线或未配置监听）")
	}
	if m.warmupInFlight[deviceID] {
		m.mu.Unlock()
		return fmt.Errorf("设备手动预热进行中，请勿重复触发")
	}
	m.warmupInFlight[deviceID] = true
	m.mu.Unlock()

	done := make(chan error, 1)
	actor.Enqueue(&ManualWarmupCmd{Providers: providers, Done: done})
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
	}
	m.mu.Lock()
	delete(m.warmupInFlight, deviceID)
	m.mu.Unlock()
	return err
}

// StopAll 停止全部设备 actor 与账户生产者/调度器（服务端退出）
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mon := range m.monitors {
		mon.Stop()
	}
	m.monitors = make(map[string]*LedgerMonitor)
	for deviceID, sched := range m.scheds {
		sched.Stop()
		delete(m.scheds, deviceID)
	}
	for deviceID, actor := range m.actors {
		actor.Stop()
		delete(m.actors, deviceID)
	}
}

// DevicePollHealth 返回各设备 poll 成功率（deviceID → 0~1；无 actor 返回 0.9 中性）。
// 均衡分配器健康度数据源（docs/PLAN-DYNAMIC-CLOSE-ALLOC.md §5.2）。
func (m *Manager) DevicePollHealth() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]float64, len(m.actors))
	for devID, actor := range m.actors {
		out[devID] = actor.PollHealth()
	}
	return out
}

// MonitorState 单账户监听状态（admin 只读接口）
type MonitorState struct {
	DeviceID   string `json:"device_id"`
	AccountID  int64  `json:"account_id"`
	Provider   string `json:"provider"`
	UserID     int    `json:"user_id"`
	Serial     string `json:"serial"`
	Active     bool   `json:"active"`      // 是否处于订单驱动轮询态（有待支付订单）
	SeenRows   int    `json:"seen_rows"`   // 已见指纹数（增量基线大小）
	CurrentCmd string `json:"current_cmd"` // 设备 actor 当前执行命令（""=空闲）
	QueueLen   int    `json:"queue_len"`   // 设备 actor 等待队列长度
	LastCmd    string `json:"last_cmd"`    // 设备 actor 最近执行命令
	LastErr    string `json:"last_err"`    // 最近命令错误（""=成功，供诊断连续失败）
}

// Status 返回全部账户监听状态（admin 只读接口）
func (m *Manager) Status() []MonitorState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MonitorState, 0, len(m.monitors))
	for _, mon := range m.monitors {
		actor := m.actors[mon.deviceID]
		cur, qlen, lastCmd, lastErr := "", 0, "", ""
		if actor != nil {
			cur = actor.CurrentCmd()
			qlen = actor.QueueLen()
			if lc, lerr := actor.LastResult(); lerr != nil {
				lastCmd, lastErr = lc, lerr.Error()
			}
		}
		out = append(out, MonitorState{
			DeviceID:   mon.deviceID,
			AccountID:  mon.accountID,
			Provider:   mon.provider,
			UserID:     mon.userId,
			Serial:     mon.serial,
			Active:     mon.IsActive(),
			SeenRows:   actorSeenRows(actor, mon.provider, mon.userId),
			CurrentCmd: cur,
			QueueLen:   qlen,
			LastCmd:    lastCmd,
			LastErr:    lastErr,
		})
	}
	return out
}

// actorSeenRows 从设备 actor 取账户已见指纹数（actor 为 nil 返回 0）
func actorSeenRows(actor *DeviceActor, provider string, userID int) int {
	if actor == nil {
		return 0
	}
	return actor.SeenRows(provider, userID)
}
