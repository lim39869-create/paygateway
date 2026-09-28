// Package ledgermon 服务端账本页 adb 监听（DeviceActor 模型，2026-08-28 重构）
//
// DeviceActor 模型：每台设备恰好一个 DeviceActor goroutine，串行消费命令队列。
// 所有 adb 操作（导航/读取/锁定/手动预热）只通过 actor 的命令执行。
// 无 lease、无仲裁、无抢占，竞态从结构上消失（docs/DESIGN-DEVICE-ACTOR.md）。
package ledgermon

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"paygateway/internal/adbmgr"
	"paygateway/internal/admin/capture"
	"paygateway/internal/logger"
)

// ========== 命令接口 ==========

// Command 设备操作命令（actor 队列元素，串行执行）
type Command interface {
	Execute(ctx context.Context, d *DeviceCtx) error
}

// ========== 命令实现 ==========

// WarmupCmd 预热：导航到指定账户账本页 + 建立基线（设备上线时入队）
type WarmupCmd struct {
	AccountID int64
	Provider  string     // alipay / wechat
	UserID    int        // 0=主空间，999=双开
	Done      chan error // 完成信号（锁定协调器/状态查询等待用）
}

func (c *WarmupCmd) Execute(ctx context.Context, d *DeviceCtx) error {
	acct := d.Account(c.Provider, c.UserID)
	if acct == nil {
		return fmt.Errorf("device_actor: 账户上下文不存在 provider=%s user_id=%d", c.Provider, c.UserID)
	}
	// 预热优先走 Warmup（task lock 热启动优先 → 账本页栈不存在时冷启动坐标导航建页）
	if w, ok := acct.Reader.(Warmer); ok {
		if err := w.Warmup(ctx); err != nil {
			logger.Warn("device_actor", "预热导航失败",
				"err", err, "device_id", d.DeviceID, "account_id", c.AccountID)
			return err
		}
	} else if err := acct.Reader.EnsureForeground(ctx); err != nil {
		logger.Warn("device_actor", "预热导航失败（无 Warmer）",
			"err", err, "device_id", d.DeviceID, "account_id", c.AccountID)
		return err
	}
	// 读取基线（冷启动导航完成时账本页即最新数据，优先用 ReadFresh 免下拉刷新）
	var rows []capture.AlipayLedgerRow
	var err error
	if fr, ok := acct.Reader.(FreshReader); ok {
		rows, err = fr.ReadFresh(ctx)
	} else {
		rows, err = acct.Reader.Read(ctx)
	}
	if err != nil {
		logger.Warn("device_actor", "预热基线读取失败",
			"err", err, "device_id", d.DeviceID, "account_id", c.AccountID)
		return err
	}
	// 空行也走 Diff：空态页是有效基线，保证首笔真实收款不被当历史吞掉
	acct.Detector.Diff(ctx, rows)
	logger.Info("device_actor", "预热完成：账本页已就绪，基线已建立",
		"device_id", d.DeviceID, "account_id", c.AccountID, "user_id", c.UserID, "rows", len(rows))
	return nil
}

// PollCmd 轮询：热启动恢复账本页 + 读取 + 增量检测 + 推送（调度器入队）。
// Done 可选（nil=投递后不等待，兼容旧调用方）；非 nil 时调度器等待执行完成（pull 模式）。
type PollCmd struct {
	AccountID int64
	Provider  string // alipay / wechat
	UserID    int    // 0=主空间，999=双开
	Done      chan error
}

func (c *PollCmd) Execute(ctx context.Context, d *DeviceCtx) error {
	acct := d.Account(c.Provider, c.UserID)
	if acct == nil {
		return nil // 账户已删除/离线，静默跳过
	}
	// 热启动恢复前台（绝不冷启动，失败本轮跳过）
	if err := acct.Reader.EnsureForeground(ctx); err != nil {
		logger.Warn("device_actor", "轮询恢复前台失败，本轮跳过",
			"err", err, "device_id", d.DeviceID, "account_id", c.AccountID)
		// 热启动连续失败 → 任务栈大概率已丢失，升级冷启动预热自愈（防永久漏检）。
		if acct.bumpPollFail() >= pollFailWarmupThreshold {
			logger.Warn("device_actor", "热启动连续失败，重投预热命令自愈（冷启动重建账本页）",
				"device_id", d.DeviceID, "account_id", c.AccountID, "failures", acct.pollFailures())
			d.Enqueue(&WarmupCmd{AccountID: c.AccountID, Provider: c.Provider, UserID: c.UserID})
		}
		d.RecordPoll(false)
		return nil
	}
	acct.resetPollFail()
	rows, err := acct.Reader.Read(ctx)
	if err != nil {
		logger.Warn("device_actor", "轮询读取失败",
			"err", err, "device_id", d.DeviceID, "account_id", c.AccountID)
		d.RecordPoll(false)
		return nil
	}
	d.RecordPoll(true)
	// 资金安全：先检测新行（Fresh 不提交 seen），推送成功后才 Commit——
	// Dispatch 失败时该行下轮仍会被检测到并重试，绝不永久丢单。
	fresh := acct.Detector.Fresh(ctx, rows)
	if len(fresh) == 0 {
		return nil
	}
	if d.Dispatch == nil {
		acct.Detector.Commit(ctx, fresh)
		return nil
	}
	if err := d.Dispatch(ctx, d.DeviceID, acct.Provider, acct.UserID, fresh); err != nil {
		logger.Error("device_actor", "收款事件推送失败（行保留待重试，不提交指纹）",
			"err", err, "device_id", d.DeviceID, "account_id", c.AccountID, "rows", len(fresh))
		return nil
	}
	acct.Detector.Commit(ctx, fresh)
	return nil
}

// ManualWarmupCmd 手动预热：完整冷启动导航 + 锁定任务（admin 触发，同步等结果）
type ManualWarmupCmd struct {
	// Providers 指定要预热的通道：{"alipay":true,"wechat":true}，空 = 全部通道
	Providers map[string]bool
	Done      chan error
}

func (c *ManualWarmupCmd) Execute(ctx context.Context, d *DeviceCtx) error {
	// 按固定顺序遍历账户（key 排序保证确定性）
	for _, key := range d.SortedAccountKeys() {
		acct := d.Accounts()[key]
		if c.Providers != nil && !c.Providers[acct.Provider] {
			continue
		}
		switch acct.Provider {
		case "alipay":
			if err := capture.CaptureAlipayLedger(ctx, d.Adb, d.Serial, acct.UserID); err != nil {
				return fmt.Errorf("支付宝预热失败 (user_id=%d): %w", acct.UserID, err)
			}
		case "wechat":
			if err := capture.CaptureWechatLedger(ctx, d.Adb, d.Serial, acct.UserID); err != nil {
				return fmt.Errorf("微信预热失败 (user_id=%d): %w", acct.UserID, err)
			}
		}
	}
	// 全部账户导航完成后锁定支付任务卡片（支付宝+微信，每应用锁主进程一张）。
	// 锁定失败不致命：热启动恢复降级为冷启动导航（EnsureForeground 兜底）。
	if _, err := capture.LockPaymentTasks(ctx, d.Adb, d.Serial, "支付宝", "微信"); err != nil {
		logger.Warn("device_actor", "手动预热后锁定任务卡片失败",
			"err", err, "device_id", d.DeviceID)
	}
	return nil
}

// LockCmd 锁定任务卡片（防一键清理；全部账户预热完成后自动入队）
type LockCmd struct {
	Done chan error
}

func (c *LockCmd) Execute(ctx context.Context, d *DeviceCtx) error {
	// 先应用命令级保活（电池白名单/后台运行豁免/禁用缓存冻结），再锁任务卡片（防一键清理）。
	// 命令级保活幂等，重复执行无害；失败不阻断锁任务（锁任务是热启动恢复的关键）。
	for _, pkg := range []string{adbmgr.WechatPkg, adbmgr.AlipayPkg} {
		if _, err := d.Adb.KeepAliveApply(ctx, d.Serial, pkg); err != nil {
			logger.Warn("device_actor", "保活命令应用失败（不阻断锁任务）",
				"err", err, "device_id", d.DeviceID, "pkg", pkg)
		}
	}
	if err := d.Adb.KeepAliveDeviceFreezer(ctx, d.Serial); err != nil {
		logger.Warn("device_actor", "禁用缓存冻结失败（不阻断锁任务）",
			"err", err, "device_id", d.DeviceID)
	}
	locked, err := capture.LockPaymentTasks(ctx, d.Adb, d.Serial, "支付宝", "微信")
	if err != nil {
		logger.Warn("device_actor", "设备任务锁定失败（热启动恢复将降级为冷启动）",
			"err", err, "device_id", d.DeviceID)
		return err
	}
	logger.Info("device_actor", "设备支付任务已全部锁定（防一键清理）",
		"device_id", d.DeviceID, "locked", locked)
	return nil
}

// ========== 设备执行上下文 ==========

// AccountCtx 账户级执行上下文（每个 (device, provider, userID) 一个）
type AccountCtx struct {
	AccountID int64
	Provider  string
	UserID    int
	Reader    LedgerReader
	Detector  *IncrementalDetector

	// pollFail 热启动连续失败次数（actor 单 goroutine 内读写，无并发）
	pollFail int
}

// pollFailWarmupThreshold 热启动连续失败升级阈值：达到后重投 WarmupCmd 冷启动自愈。
// 3 次 ≈ 3 个轮询周期（1s 间隔），任务栈丢失时热启动必然失败，及时冷启动恢复。
const pollFailWarmupThreshold = 3

// warmupRetryDelay 设备上线预热失败后的重试退避间隔。预热建基线是必要动作，
// 偶发失败（uiautomator dump 冷启动被 kill）后短暂退避重试，避免立即重试撞同一次故障。
const warmupRetryDelay = 5 * time.Second

func (a *AccountCtx) bumpPollFail() int { a.pollFail++; return a.pollFail }
func (a *AccountCtx) pollFailures() int { return a.pollFail }
func (a *AccountCtx) resetPollFail()    { a.pollFail = 0 }

// DeviceCtx 设备级执行上下文。Adb/Serial 供 ManualWarmupCmd/LockCmd 直连 capture。
type DeviceCtx struct {
	DeviceID string
	Serial   string
	Adb      *adbmgr.Manager
	Dispatch DispatchFunc
	// actor 设备执行者（命令自愈时向队列投递新命令用，避免重入锁）
	actor *DeviceActor
	// accounts 账户上下文快照（atomic.Pointer：reconcile 写、actor 读无锁，杜绝 map 并发读写崩溃）
	accounts atomic.Pointer[map[string]*AccountCtx]
}

// Enqueue 向本设备命令队列投递命令（自愈等 actor 内部路径使用）
func (d *DeviceCtx) Enqueue(cmd Command) { d.actor.Enqueue(cmd) }

// RecordPoll 记录一次 poll 成败（设备健康度滑动窗口，均衡分配器数据源）
func (d *DeviceCtx) RecordPoll(ok bool) {
	if d.actor != nil {
		d.actor.RecordPoll(ok)
	}
}

// Accounts 返回账户上下文快照（不可变 map，安全并发读）
func (d *DeviceCtx) Accounts() map[string]*AccountCtx {
	return *d.accounts.Load()
}

// Account 按 provider+userID 查找账户上下文
func (d *DeviceCtx) Account(provider string, userID int) *AccountCtx {
	return (*d.accounts.Load())[monitorKey(d.DeviceID, provider, userID)]
}

// SortedAccountKeys 返回按 key 排序的账户列表（保证执行顺序确定性）
func (d *DeviceCtx) SortedAccountKeys() []string {
	accts := d.Accounts()
	keys := make([]string, 0, len(accts))
	for k := range accts {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i] > keys[j] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// ========== DeviceActor ==========

// maxQueue 命令队列上限：WarmupCmd/PollCmd 队满（>maxQueue）时丢弃 PollCmd
// （跳过一轮，资金安全优先），保留 WarmupCmd/ManualWarmupCmd。
const maxQueue = 16

// DeviceActor 每设备单执行者：串行消费命令队列，唯一 adb 访问者。
type DeviceActor struct {
	DeviceID  string
	DeviceCtx *DeviceCtx

	mu          sync.Mutex
	queue       []Command
	pendingPoll map[int64]bool // 已在队列中的 PollCmd（按 accountID 去重）
	cond        *sync.Cond

	current atomic.Value // string：当前执行命令名（可观测），空=空闲
	last    atomic.Value // lastResult：最近一次命令执行结果（可观测）
	stop    chan struct{}
	done    chan struct{}

	// health 设备 poll 健康度滑动窗口（均衡分配器数据源，docs/PLAN-DYNAMIC-CLOSE-ALLOC.md §6）
	health *healthWindow
}

// healthWindow 设备 poll 成功率滑动窗口（最近 size 次，无历史中性 0.9）。
// 并发安全（RecordPoll 由 actor goroutine + 分配器读并发调用）。
type healthWindow struct {
	mu     sync.Mutex
	ok     []bool
	next   int
	filled bool
	size   int
}

func newHealthWindow(size int) *healthWindow {
	if size <= 0 {
		size = 20
	}
	return &healthWindow{ok: make([]bool, size), size: size}
}

func (h *healthWindow) record(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ok[h.next] = ok
	h.next = (h.next + 1) % h.size
	if h.next == 0 {
		h.filled = true
	}
}

func (h *healthWindow) health() float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.filled {
		n := h.next
		if n == 0 {
			return 0.9 // 无历史：中性，不因无数据饿死新设备
		}
		sum := 0
		for i := 0; i < n; i++ {
			if h.ok[i] {
				sum++
			}
		}
		return float64(sum) / float64(n)
	}
	sum := 0
	for _, v := range h.ok {
		if v {
			sum++
		}
	}
	return float64(sum) / float64(h.size)
}

// cmdName 返回命令的简短描述（可观测）
func cmdName(cmd Command) string {
	switch cmd.(type) {
	case *WarmupCmd:
		return "warmup"
	case *PollCmd:
		return "poll"
	case *ManualWarmupCmd:
		return "manual_warmup"
	case *LockCmd:
		return "lock"
	default:
		return "unknown"
	}
}

// NewDeviceActor 创建设备执行者并启动后台 goroutine。
func NewDeviceActor(deviceID, serial string, adb *adbmgr.Manager, dispatch DispatchFunc) *DeviceActor {
	accounts := make(map[string]*AccountCtx)
	a := &DeviceActor{
		DeviceID: deviceID,
		DeviceCtx: &DeviceCtx{
			DeviceID: deviceID,
			Serial:   serial,
			Adb:      adb,
			Dispatch: dispatch,
		},
		pendingPoll: make(map[int64]bool),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		health:      newHealthWindow(0),
	}
	a.DeviceCtx.actor = a
	a.DeviceCtx.accounts.Store(&accounts)
	a.cond = sync.NewCond(&a.mu)
	a.current.Store("")
	a.last.Store(lastResult{})
	go a.loop()
	return a
}

// AddAccount 添加账户上下文（reconcile 时调用；幂等：已存在时覆盖）。
// 写路径：锁内复制 map 快照后原子替换——读方（actor 命令执行）无锁 Load，无数据竞态。
func (a *DeviceActor) AddAccount(acct *AccountCtx) {
	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.DeviceCtx.Accounts()
	next := make(map[string]*AccountCtx, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[monitorKey(a.DeviceID, acct.Provider, acct.UserID)] = acct
	a.DeviceCtx.accounts.Store(&next)
}

// RemoveAccount 移除账户上下文（设备离线时调用）
func (a *DeviceActor) RemoveAccount(key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.DeviceCtx.Accounts()
	if _, ok := old[key]; !ok {
		return
	}
	next := make(map[string]*AccountCtx, len(old))
	for k, v := range old {
		if k != key {
			next[k] = v
		}
	}
	a.DeviceCtx.accounts.Store(&next)
}

// Enqueue 投递命令到队列（非阻塞）。
//   - PollCmd：同账户去重（已有在队则合并）；队满（>maxQueue）时丢弃（宁可跳过一轮）。
//   - ManualWarmupCmd：插队到队首（手动操作优先，但不打断执行中的命令——只影响排队顺序）。
//   - WarmupCmd/LockCmd：正常入队（数量有限，不丢弃）。
func (a *DeviceActor) Enqueue(cmd Command) {
	a.mu.Lock()
	defer a.mu.Unlock()

	switch c := cmd.(type) {
	case *PollCmd:
		if a.pendingPoll[c.AccountID] {
			return // 同账户已在队中，合并（防下单风暴堆积）
		}
		if len(a.queue) >= maxQueue {
			logger.Warn("device_actor", "命令队列已满，丢弃 PollCmd",
				"device_id", a.DeviceID, "account_id", c.AccountID)
			return
		}
		a.pendingPoll[c.AccountID] = true
		a.queue = append(a.queue, cmd)
	case *ManualWarmupCmd:
		// 插队到队首：手动操作优先，但执行中的命令不被打断（原子性保证）
		a.queue = append([]Command{cmd}, a.queue...)
	default:
		a.queue = append(a.queue, cmd)
	}
	a.cond.Signal()
}

// CurrentCmd 返回当前执行命令名（可观测）
func (a *DeviceActor) CurrentCmd() string {
	s, _ := a.current.Load().(string)
	return s
}

// QueueLen 返回当前队列长度（可观测）
func (a *DeviceActor) QueueLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.queue)
}

// Stop 停止执行者（等待当前命令执行完）。队列中残留命令不再执行，
// 其 Done 等待者（Monitor.warmupOnce/WarmupDevice）由 loop 退出时统一唤醒，避免永久阻塞。
func (a *DeviceActor) Stop() {
	close(a.stop)
	a.cond.Broadcast()
	<-a.done
}

// loop 执行循环：串行取命令 → 执行 → 唤醒等待者
func (a *DeviceActor) loop() {
	defer close(a.done)
	ctx := context.Background()
	for {
		cmd := a.pop()
		if cmd == nil {
			// 已停止：唤醒队列中残留命令的 Done 等待者（不再执行，直接报错返回）
			a.abortQueued()
			return
		}
		name := cmdName(cmd)
		a.current.Store(name)
		err := cmd.Execute(ctx, a.DeviceCtx)
		a.setLastResult(name, err)
		// 先记录结果再唤醒等待者：Status 能看到最后命令及其错误
		signalCmdDone(cmd, err)
		a.current.Store("")
	}
}

// abortQueued 停止时对队列中所有残留命令的 Done 等待者回发错误（防永久阻塞）。
func (a *DeviceActor) abortQueued() {
	a.mu.Lock()
	queue := a.queue
	a.queue = nil
	a.mu.Unlock()
	for _, cmd := range queue {
		signalCmdDone(cmd, fmt.Errorf("device_actor: 设备执行者已停止，命令未执行"))
	}
}

// lastResult 最近一次执行结果（可观测：Status 展示，供 admin 诊断连续失败）
type lastResult struct {
	cmd string
	err error
	at  time.Time
}

func (a *DeviceActor) setLastResult(cmd string, err error) {
	a.last.Store(lastResult{cmd: cmd, err: err, at: time.Now()})
}

// LastResult 返回最近一次命令执行结果（Status 展示）
func (a *DeviceActor) LastResult() (string, error) {
	lr := a.last.Load().(lastResult)
	return lr.cmd, lr.err
}

// RecordPoll 记录一次 poll 成败（均衡分配器健康度数据源）
func (a *DeviceActor) RecordPoll(ok bool) {
	if a.health != nil {
		a.health.record(ok)
	}
}

// PollHealth 设备 poll 成功率（0~1；无历史返回 0.9 中性）
func (a *DeviceActor) PollHealth() float64 {
	if a.health == nil {
		return 0.9
	}
	return a.health.health()
}

// pop 阻塞等待直到有命令可执行或收到停止信号。
// 每轮先查 stop 再取命令——停止后不再执行队列中残留命令（Stop 只等当前命令，不排空队列）。
func (a *DeviceActor) pop() Command {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		select {
		case <-a.stop:
			return nil
		default:
		}
		if len(a.queue) > 0 {
			cmd := a.queue[0]
			a.queue = a.queue[1:]
			if pc, ok := cmd.(*PollCmd); ok {
				delete(a.pendingPoll, pc.AccountID)
			}
			return cmd
		}
		a.cond.Wait()
	}
}

// signalCmdDone 通知命令完成（幂等：无 Done 通道的命令直接返回）
func signalCmdDone(cmd Command, err error) {
	var done chan error
	switch c := cmd.(type) {
	case *WarmupCmd:
		done = c.Done
	case *PollCmd:
		done = c.Done
	case *ManualWarmupCmd:
		done = c.Done
	case *LockCmd:
		done = c.Done
	}
	if done != nil {
		done <- err // 缓冲 1，actor 不回读，不会阻塞
	}
}

// SeenRows 返回某账户已见指纹数（增量基线大小，Status 展示）
func (a *DeviceActor) SeenRows(provider string, userID int) int {
	if acct := a.DeviceCtx.Account(provider, userID); acct != nil && acct.Detector != nil {
		return acct.Detector.Size()
	}
	return 0
}
