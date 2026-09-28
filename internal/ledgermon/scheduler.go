// DeviceScheduler 设备级公平时间占比弹性调度器（docs/DESIGN-FAIR-SCHEDULER.md）
//
// 职责：每设备恰好一个调度 goroutine，是唯一 PollCmd 生产者；DeviceActor 保持
// 被动串行执行者（命令队列不变）。调度输入为 WaitPayPool 内存订单池快照（零锁）。
//
// 核心语义（2026-08-29 与用户确认）：
//   - 记账单位 = 一次完整 poll（原子不可打断，绝不按墙钟固定时间切片）
//   - 顺序维度（前台处理权）：等待最久的账户先做（age 降序）
//   - 配额维度（时间使用权）：与订单数量挂钩（q ∝ count，floor=1，cap=60%）
//   - 硬紧急：minTTL < UrgentTTL 的账户无条件置顶（防关单丢单）
//   - 窗口动态：窗口 = 一组配额 poll 操作的集合，墙钟长度随单次 poll 实测时长浮动
//   - 反馈：每 poll 后查池，账户池空立即提前终止该 visit；窗口结束立即重算
package ledgermon

import (
	"context"
	"math"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"paygateway/internal/logger"
	"paygateway/internal/pool"
)

// SchedulerConfig 调度器参数（默认值见 DefaultSchedulerConfig；可注入覆盖）
type SchedulerConfig struct {
	// BaseQuota Q_total 基数：窗口总配额 = 活跃账户数 × BaseQuota（默认 3）
	BaseQuota int
	// CapPct 单账户占整轮窗口配额上限比例（默认 0.6）
	CapPct float64
	// DwellMax 单账户连续 poll 上限（强制看一眼其它账户，默认 3）
	DwellMax int
	// UrgentTTL 硬紧急阈值：minTTL < UrgentTTL 的账户无条件置顶（默认 8s ≈ 2×单次 poll）
	UrgentTTL time.Duration
	// IdleSleep 池空休眠间隔（下单唤醒前的最小探测周期，默认 500ms）
	IdleSleep time.Duration
	// PollMinGap / PollMaxGap 相邻两次账单页 poll 之间的随机间隔（默认 1s~3s，时间驱动）。
	// 只对「有待支付订单的账户」生效（plan 已限定 Count>0 账户，无订单账户不刷）；
	// 随机化打破固定节奏，避免支付宝/微信账单页按恒定频率被检测为抓取轰炸。
	// PollMaxGap < PollMinGap 或 PollMaxGap<=0 时退化为无间隔（测试用，极低延迟）。
	PollMinGap time.Duration
	PollMaxGap time.Duration
	// AccountPoller 按 (provider,userID) 执行单次 poll 的钩子（注入 PollCmd 到 actor）
	AccountPoller func(ctx context.Context, provider string, userID int) error
}

// DefaultSchedulerConfig 默认参数（依据 DESIGN-FAIR-SCHEDULER.md §5.3 参数表）
func DefaultSchedulerConfig() SchedulerConfig {
	return SchedulerConfig{
		BaseQuota:     3,
		CapPct:        0.6,
		DwellMax:      3,
		UrgentTTL:     8 * time.Second,
		IdleSleep:     500 * time.Millisecond,
		PollMinGap:    1 * time.Second,
		PollMaxGap:    3 * time.Second,
		AccountPoller: nil, // 由 NewDeviceScheduler 注入 actor 封装
	}
}

// schedulerAcct 调度器视图中的账户（provider/userID 从 actor 账户快照解析）
type schedulerAcct struct {
	accountID int64
	provider  string
	userID    int
}

// DeviceScheduler 设备级调度器：窗口决策循环（决策只在 poll 边界发生）
type DeviceScheduler struct {
	deviceID string
	pool     *pool.WaitPayPool
	cfg      SchedulerConfig
	// accounts 本设备账户快照（reconcile 写入，调度循环读取；atomic 防竞态）
	accounts atomic.Value // []schedulerAcct
	// activeIDs 当前活跃账户（池快照中 count>0 的账户，按窗口重算）
	// 调度循环内部使用，无需锁（单 goroutine）

	wake chan struct{} // 下单唤醒（非阻塞；池空休眠时立即响应）
	stop chan struct{}
	done chan struct{}

	// 可观测（admin Status）
	current atomic.Value // 当前窗口描述（windowDesc）

	// roundStat 窗口耗时滑动统计（设备一轮完整轮询实际耗时，供 poll 成本校准/超限判定）
	roundStat *roundStat
}

// roundStat 窗口耗时滑动平均（最近 N 轮）。
type roundStat struct {
	mu   sync.Mutex
	vals []time.Duration
	next int
	full bool
	size int
}

func newRoundStat(size int) *roundStat {
	if size <= 0 {
		size = 10
	}
	return &roundStat{vals: make([]time.Duration, size), size: size}
}

func (r *roundStat) record(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vals[r.next] = d
	r.next = (r.next + 1) % r.size
	if r.next == 0 {
		r.full = true
	}
}

// avg 返回滑动平均耗时（无数据返回 0）。
func (r *roundStat) avg() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		n := r.next
		if n == 0 {
			return 0
		}
		var sum time.Duration
		for i := 0; i < n; i++ {
			sum += r.vals[i]
		}
		return sum / time.Duration(n)
	}
	var sum time.Duration
	for _, v := range r.vals {
		sum += v
	}
	return sum / time.Duration(r.size)
}

// windowDesc 当前窗口可观测状态
type windowDesc struct {
	Active int     // 本窗口活跃账户数
	Total  int     // 本窗口总配额
	Seq    []int64 // 本窗口执行顺序（accountID）
	Quota  map[int64]int
	At     time.Time
}

// NewDeviceScheduler 创建设备调度器（不启动 goroutine；调用 Start()）
// poll 由 actor 封装：投递带 Done 的 PollCmd 并等待执行完成（pull 模式）
func NewDeviceScheduler(deviceID string, actor *DeviceActor, p *pool.WaitPayPool, cfg SchedulerConfig) *DeviceScheduler {
	if cfg.BaseQuota <= 0 {
		cfg.BaseQuota = 3
	}
	if cfg.CapPct <= 0 || cfg.CapPct >= 1 {
		cfg.CapPct = 0.6
	}
	if cfg.DwellMax <= 0 {
		cfg.DwellMax = 3
	}
	if cfg.UrgentTTL <= 0 {
		cfg.UrgentTTL = 8 * time.Second
	}
	if cfg.IdleSleep <= 0 {
		cfg.IdleSleep = 500 * time.Millisecond
	}
	if cfg.PollMaxGap <= 0 || cfg.PollMaxGap < cfg.PollMinGap {
		// 无有效间隔（测试置 0）→ 退化为不等待（极低延迟，与旧 execute 逐字节一致）
		cfg.PollMinGap, cfg.PollMaxGap = 0, 0
	} else if cfg.PollMinGap <= 0 {
		cfg.PollMinGap = 1 * time.Second
	}
	s := &DeviceScheduler{
		deviceID:  deviceID,
		pool:      p,
		cfg:       cfg,
		wake:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		roundStat: newRoundStat(10),
	}
	s.accounts.Store([]schedulerAcct{})
	s.current.Store(windowDesc{})
	// 注入默认 poller：经 actor 串行执行（Done 等待 = pull）
	if cfg.AccountPoller == nil {
		s.cfg.AccountPoller = func(ctx context.Context, provider string, userID int) error {
			acct := actor.DeviceCtx.Account(provider, userID)
			if acct == nil {
				return nil // 账户已删除/离线，静默跳过
			}
			done := make(chan error, 1)
			actor.Enqueue(&PollCmd{AccountID: acct.AccountID, Provider: provider, UserID: userID, Done: done})
			select {
			case err := <-done:
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return s
}

// SetAccounts 更新本设备账户快照（reconcile 调用）
func (s *DeviceScheduler) SetAccounts(accts []schedulerAcct) {
	s.accounts.Store(accts)
}

// Wake 下单唤醒（非阻塞；池空休眠时立即响应）
func (s *DeviceScheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Start 启动调度 goroutine
func (s *DeviceScheduler) Start() {
	go s.loop()
}

// Stop 停止调度器（等待退出）
func (s *DeviceScheduler) Stop() {
	close(s.stop)
	<-s.done
}

// loop 窗口决策循环：S1读池→S2配额→S3排序→S4执行→S5重算（池空休眠）
func (s *DeviceScheduler) loop() {
	defer close(s.done)
	ctx := context.Background()
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		// 设备维度：只读本设备子池快照（多设备天然隔离，互不干扰）
		stat := s.pool.DeviceSnapshot(s.deviceID)
		order, quotas := s.plan(stat)
		if len(order) == 0 {
			// 无活跃账户：休眠等待（下单唤醒或周期探测）
			select {
			case <-s.stop:
				return
			case <-s.wake:
			case <-time.After(s.cfg.IdleSleep):
			}
			continue
		}
		s.publishWindow(order, quotas)
		s.execute(ctx, order, quotas)
		// 每笔 poll 已在 execute 内按 PollMinGap~PollMaxGap 随机间隔（默认1~3s）执行，
		// 此处只需保证 stop/wake 响应，不再额外休眠固定时长（否则会稀释 1~3s 的节奏）。
		select {
		case <-s.stop:
			return
		case <-s.wake:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// plan 纯调度决策：输入设备子池快照 → (执行顺序, 每账户配额)
// 配额：q_a = max(1, min(Q_total × count_a/Σcount, Q_total×CapPct))
// 顺序：硬紧急（minTTL<UrgentTTL）置顶按 ttl 升序；其余按 age 降序（等最久的先做）
// DeviceStat.Accounts 已限定本设备账户（设备子池内聚合），无需全局过滤。
func (s *DeviceScheduler) plan(stat *pool.DeviceStat) ([]int64, map[int64]int) {
	if stat == nil || stat.Total == 0 || len(stat.Accounts) == 0 {
		return nil, nil
	}
	// 活跃账户：仅同设备同账户出现同金额订单(DupAmount)才轮询账单页。
	// 金额互异的订单由通知栏按金额唯一匹配入账，不刷账本页（避免抓取轰炸风控）。
	active := make([]*pool.AccountStat, 0, len(stat.Accounts))
	for i := range stat.Accounts {
		st := &stat.Accounts[i]
		if st.Count > 0 && st.DupAmount {
			active = append(active, st)
		}
	}
	if len(active) == 0 {
		return nil, nil
	}
	// S2 配额
	qTotal := len(active) * s.cfg.BaseQuota
	totalOrders := 0
	for _, a := range active {
		totalOrders += a.Count
	}
	capQuota := int(math.Floor(float64(qTotal) * s.cfg.CapPct))
	if capQuota < 1 {
		capQuota = 1
	}
	quotas := make(map[int64]int, len(active))
	for _, a := range active {
		q := 1 // floor：至少 1 次
		if totalOrders > 0 {
			q = int(math.Round(float64(qTotal) * float64(a.Count) / float64(totalOrders)))
		}
		if q < 1 {
			q = 1
		}
		if q > capQuota {
			q = capQuota
		}
		quotas[a.AccountID] = q
	}
	// S3 排序
	sort.Slice(active, func(i, j int) bool {
		ui, uj := active[i].MinTTL < s.cfg.UrgentTTL, active[j].MinTTL < s.cfg.UrgentTTL
		switch {
		case ui && !uj:
			return true // 硬紧急置顶
		case !ui && uj:
			return false
		case ui && uj:
			return active[i].MinTTL < active[j].MinTTL // 紧急内按 ttl 升序
		default:
			if active[i].OldestAge != active[j].OldestAge {
				return active[i].OldestAge > active[j].OldestAge // 等最久的先做
			}
			return active[i].AccountID < active[j].AccountID // 确定性
		}
	})
	order := make([]int64, 0, len(active))
	for _, a := range active {
		order = append(order, a.AccountID)
	}
	return order, quotas
}

// execute 按顺序执行：每账户连续 poll ≤ min(配额, DwellMax)；每 poll 后查池，
// 账户池空 → 提前终止该 visit（防空转）；命令执行失败不致命（下窗口重试）。
func (s *DeviceScheduler) execute(ctx context.Context, order []int64, quotas map[int64]int) {
	start := time.Now()
	defer func() { s.roundStat.record(time.Since(start)) }()
	accts := s.accounts.Load().([]schedulerAcct)
	byID := make(map[int64]*schedulerAcct, len(accts))
	for i := range accts {
		a := &accts[i]
		byID[a.accountID] = a
	}
	for _, acctID := range order {
		acct := byID[acctID]
		if acct == nil {
			continue
		}
		quota := quotas[acctID]
		served := 0
		for served < quota {
			if served >= s.cfg.DwellMax {
				break // 连续占用上限：强制看一眼其它账户
			}
			select {
			case <-s.stop:
				return
			default:
			}
			// 时间驱动 + 随机间隔：每次账单页 poll 前均匀随机等待 [PollMinGap, PollMaxGap]（默认1~3s），
			// 打破固定节奏，避免微信/支付宝账单页被检测为抓取轰炸（风控）。仅同金额订单账户进入本循环。
			if g := s.pollGap(); g > 0 {
				select {
				case <-s.stop:
					return
				case <-time.After(g):
				}
			}
			if err := s.cfg.AccountPoller(ctx, acct.provider, acct.userID); err != nil {
				logger.Warn("ledgermon", "调度 poll 执行失败（下窗口重试）",
					"device_id", s.deviceID, "account_id", acctID, "err", err)
			}
			served++
			// 反馈：同金额订单歧义解除（该账户不再有 ≥2 笔同金额待支付）→ 提前终止本 visit
			if !s.pool.HasDupAmount(acctID) {
				break
			}
		}
	}
}

// pollGap 返回相邻两次账单页 poll 之间的随机等待时长：均匀分布于 [PollMinGap, PollMaxGap]。
// 随机化打破固定节奏（防微信/支付宝账单页被检测为抓取轰炸风控）。PollMaxGap<=0（测试退化）返回 0。
func (s *DeviceScheduler) pollGap() time.Duration {
	if s.cfg.PollMaxGap <= 0 {
		return 0
	}
	d := int64(s.cfg.PollMaxGap - s.cfg.PollMinGap)
	if d <= 0 {
		return s.cfg.PollMinGap
	}
	return s.cfg.PollMinGap + time.Duration(rand.Int63n(d))
}

// publishWindow 发布当前窗口可观测状态
func (s *DeviceScheduler) publishWindow(order []int64, quotas map[int64]int) {
	seq := make([]int64, len(order))
	copy(seq, order)
	q := make(map[int64]int, len(quotas))
	for k, v := range quotas {
		q[k] = v
	}
	s.current.Store(windowDesc{Active: len(order), Total: s.windowTotal(quotas), Seq: seq, Quota: q, At: time.Now()})
}

// windowTotal 窗口总配额
func (s *DeviceScheduler) windowTotal(quotas map[int64]int) int {
	t := 0
	for _, v := range quotas {
		t += v
	}
	return t
}

// CurrentWindow 当前窗口描述（admin 可观测）
func (s *DeviceScheduler) CurrentWindow() windowDesc {
	d, _ := s.current.Load().(windowDesc)
	return d
}

// AvgRound 窗口平均耗时（最近 N 轮，poll 成本校准用；无数据返回 0）
func (s *DeviceScheduler) AvgRound() time.Duration {
	return s.roundStat.avg()
}
