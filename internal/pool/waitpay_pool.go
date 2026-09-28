// Package pool 提供 WAIT_PAY 订单的并发安全内存池。
//
// 设计要点：
//   - 设备维度子池：devices[deviceID] 一个设备子池，多设备天然隔离、互不干扰，
//     每台设备只处理自己账户的订单（docs/PLAN-DYNAMIC-CLOSE-ALLOC.md §3）；
//   - 单一 sync.RWMutex 守护全部索引表（写入低频），聚合快照通过 atomic.Pointer 持有，读路径零锁；
//   - 动态关单窗口：CloseAt 由注入的 winCalc（设备负载驱动）计算，单调只增（防误关）；
//   - 本包不依赖 store/order 等业务包，避免反向依赖循环；仅依赖 model（领域类型）和 logger（结构化告警）。
package pool

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"paygateway/internal/logger"
	"paygateway/internal/model"
)

// 默认关单窗口：closeWindowMinutes<=0 且未注入 winCalc 时使用
const defaultCloseWindow = 30 * time.Minute

// Entry 池中订单条目（不可变值对象，Add/Upsert 时由调用方持有原对象；本包不会修改）。
type Entry struct {
	OutTradeNo       string
	AccountID        int64
	DeviceID         string // 所属设备（账户→设备映射解析；未知归 ""）
	Channel          string // personal.alipay / personal.wechat
	AmountMinor      int64  // 分
	NoteCode         string // 备注码（可空）
	CreatedAt        time.Time
	CloseAt          time.Time // 预计关单时间 = CreatedAt + 动态窗口（单调只增）
	SessionExpiresAt time.Time // 支付会话硬截止时间，替换订单不得延长
}

// AccountStat 单账户聚合统计（快照元素，值类型不可变）。
type AccountStat struct {
	AccountID int64
	Count     int
	// DupAmount 同设备同账户是否存在 ≥2 笔相同金额的 WAIT_PAY 订单；
	// 决定调度器是否需轮询账单页（金额互异的订单由通知栏按金额唯一匹配入账）。
	DupAmount bool
	OldestAge time.Duration // 最久订单已等待时长（>0）
	MinTTL    time.Duration // 距最早关单剩余时间（可为负=已超时）
}

// Snapshot 全池聚合快照（原子指针持有，不可变；notifywatcher 等全局判断用）。
type Snapshot struct {
	Accounts  []AccountStat
	Total     int
	UpdatedAt time.Time
}

// DeviceStat 单设备聚合快照（设备调度器/均衡分配器零锁读取）。
type DeviceStat struct {
	DeviceID    string
	Total       int           // 该设备 WAIT_PAY 总数（设备负载）
	Accounts    []AccountStat // 该设备内按账户聚合
	ActiveAccts int           // 活跃账户数（Count>0）
	EstRound    time.Duration // 估算一轮耗时（ActiveAccts × 配额 × pollCost，供动态窗口/超限判定）
	CloseWindow time.Duration // 当前生效动态关单窗口（最新 Add 时计算）
	UpdatedAt   time.Time
}

// devicePool 单设备子池：三张索引表（与旧全局结构同构，按设备隔离）。
type devicePool struct {
	buckets  map[int64]map[string]*Entry
	byNote   map[int64]map[string]string
	byAmount map[int64]map[int64]map[string]struct{}
	stat     atomic.Pointer[DeviceStat]
}

// DeviceOfFunc 账户 → 设备 解析（main.go 注入：personal.db 设备绑定）。
// ok=false 时订单归入 "" 兜底桶（无设备绑定/解析失败，Reconcile 修复）。
type DeviceOfFunc func(accountID int64) (deviceID string, ok bool)

// WindowCalcFunc 动态关单窗口计算器（main.go 注入；nil = 固定 fallback 窗口）。
// 入参：该设备当前活跃账户数与待支付订单总数（Add 时点快照）。
// 返回：订单关单窗口（实现方负责 clamp 到 [MIN, MAX]）。
type WindowCalcFunc func(activeAccounts, total int) time.Duration

// WaitPayPool 并发安全的 WAIT_PAY 订单池。
//
// 内部数据结构：
//   - devices[deviceID].buckets[accountID][outTradeNo] = *Entry
//   - devices[deviceID].byNote[accountID][noteCode] = outTradeNo（noteCode 为空时不入索引）
//   - devices[deviceID].byAmount[accountID][amountMinor][outTradeNo] = struct{}{}
//   - acctDev[accountID] = deviceID（账户→设备反查，匹配/Count/移除零遍历）
//
// 所有表必须在写锁内同步变更；快照由 rebuildStatsLocked 在写锁内一次性生成。
type WaitPayPool struct {
	deviceOf DeviceOfFunc
	winCalc  WindowCalcFunc
	fallback time.Duration // winCalc 为 nil 时的固定关窗（closeWindowMinutes）

	mu      sync.RWMutex
	devices map[string]*devicePool
	acctDev map[int64]string // accountID → deviceID

	snap atomic.Pointer[Snapshot]
}

// NewWaitPayPool 创建池。closeWindowMinutes<=0 且未注入 winCalc 时 CloseAt 用 CreatedAt+30min 兜底。
func NewWaitPayPool(closeWindowMinutes int) *WaitPayPool {
	w := defaultCloseWindow
	if closeWindowMinutes > 0 {
		w = time.Duration(closeWindowMinutes) * time.Minute
	}
	p := &WaitPayPool{
		fallback: w,
		devices:  make(map[string]*devicePool),
		acctDev:  make(map[int64]string),
	}
	p.devices[""] = newDevicePool()
	p.publishGlobalLocked()
	return p
}

// newDevicePool 创建空设备子池并初始化空快照。
func newDevicePool() *devicePool {
	dp := &devicePool{
		buckets:  make(map[int64]map[string]*Entry),
		byNote:   make(map[int64]map[string]string),
		byAmount: make(map[int64]map[int64]map[string]struct{}),
	}
	dp.stat.Store(&DeviceStat{})
	return dp
}

// SetDeviceOf 注入账户→设备解析（main.go 在设备增删时调用；可重复注入）。
func (p *WaitPayPool) SetDeviceOf(fn DeviceOfFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deviceOf = fn
	// 注入后全量重分桶：条目按新映射重新解析设备归属（DeviceID 先清空再走新 deviceOf）
	if fn != nil {
		var all []*Entry
		for _, dp := range p.devices {
			for _, m := range dp.buckets {
				for _, e := range m {
					e.DeviceID = ""
					all = append(all, e)
				}
			}
		}
		p.devices = make(map[string]*devicePool)
		p.acctDev = make(map[int64]string)
		p.devices[""] = newDevicePool()
		for _, e := range all {
			p.indexLocked(e)
		}
	}
	p.publishGlobalLocked()
}

// SetWindowCalc 注入动态关单窗口计算器（main.go 调用；nil = 固定 fallback）。
// 注入后对池内现有条目重新计算 CloseAt（仅延长不缩短，保持单调）。
func (p *WaitPayPool) SetWindowCalc(fn WindowCalcFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.winCalc = fn
	for _, dp := range p.devices {
		for _, m := range dp.buckets {
			for _, e := range m {
				devID := p.deviceOfEntry(e)
				nw := p.windowFor(devID)
				if nw > 0 && e.CloseAt.Before(e.CreatedAt.Add(nw)) {
					e.CloseAt = e.CreatedAt.Add(nw)
				}
			}
		}
	}
	p.publishGlobalLocked()
}

// deviceOfEntry 返回条目所属设备（优先 entry.DeviceID；兜底 deviceOf 解析）。
func (p *WaitPayPool) deviceOfEntry(e *Entry) string {
	if e.DeviceID != "" {
		return e.DeviceID
	}
	if p.deviceOf != nil {
		if d, ok := p.deviceOf(e.AccountID); ok && d != "" {
			return d
		}
	}
	return ""
}

// windowFor 计算某设备当前动态关单窗口：winCalc(活跃账户数, 待支付订单数)。
// 活跃账户数 = 该设备子池中 Count>0 的账户数（不含待添加条目本身）。
func (p *WaitPayPool) windowFor(deviceID string) time.Duration {
	if p.winCalc == nil {
		return p.fallback
	}
	dp := p.devices[deviceID]
	if dp == nil {
		return p.fallback
	}
	active, total := 0, 0
	for _, m := range dp.buckets {
		if len(m) > 0 {
			active++
			total += len(m)
		}
	}
	w := p.winCalc(active, total)
	if w <= 0 {
		return p.fallback
	}
	return w
}

// orderToEntry 从 model.Order 构造池内 Entry。
// CloseAt = CreatedAt + 动态窗口（Add 时点设备负载计算）；单调只增（Upsert 覆盖时 CloseAt 不倒退）。
func (p *WaitPayPool) orderToEntry(o *model.Order) *Entry {
	devID := ""
	if p.deviceOf != nil {
		if d, ok := p.deviceOf(o.AccountID); ok {
			devID = d
		}
	}
	closeAt := o.CreatedAt.Add(p.windowFor(devID))
	if !o.SessionExpiresAt.IsZero() && o.SessionExpiresAt.Before(closeAt) {
		closeAt = o.SessionExpiresAt
	}
	return &Entry{
		OutTradeNo:       o.OutTradeNo,
		AccountID:        o.AccountID,
		DeviceID:         devID,
		Channel:          o.Channel,
		AmountMinor:      o.AmountMinor,
		NoteCode:         o.NoteCode,
		CreatedAt:        o.CreatedAt,
		CloseAt:          closeAt,
		SessionExpiresAt: o.SessionExpiresAt,
	}
}

// Add 加入池。幂等：同 outTradeNo 已存在则覆盖更新（NoteCode/Amount/Channel 等变更会被刷新）。
// CloseAt 单调只增：覆盖时若新窗口计算的 CloseAt 早于旧值，保持旧值（防负载下降提前关单误关）。
func (p *WaitPayPool) Add(o *model.Order) {
	if o == nil || o.OutTradeNo == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	entry := p.orderToEntry(o)

	// 若已存在：先从旧索引中移除，再用新 entry 写入
	if old, ok := p.lookupLocked(o.OutTradeNo); ok {
		if entry.CloseAt.Before(old.CloseAt) {
			entry.CloseAt = old.CloseAt // 单调只增
		}
		p.unindexLocked(old)
		// 显式删除旧桶条目（unindexLocked 只清 byNote/byAmount，buckets 由调用方删）。
		// 设备映射变更时（旧 DeviceID ≠ 新 DeviceID）必须删旧桶，否则同订单残留双设备子池，
		// 导致旧设备负载虚高、均衡分配失真（P2-3）。
		if od, ok := p.devices[old.DeviceID]; ok {
			if m, ok := od.buckets[old.AccountID]; ok {
				delete(m, old.OutTradeNo)
				if len(m) == 0 {
					delete(od.buckets, old.AccountID)
					delete(p.acctDev, old.AccountID)
				}
			}
		}
	}
	p.indexLocked(entry)
	p.publishGlobalLocked()
}

// Upsert 覆盖语义别名。Add 本身已支持幂等覆盖（refresh credential / channel 时 NoteCode 可能变化）。
func (p *WaitPayPool) Upsert(o *model.Order) {
	p.Add(o)
}

// CloseAtOf 查询订单的动态关单时间（order.Service 注入 WaitPayHooks.CloseAtOf 用）。
// 返回 (closeAt, found)。池中不存在 → found=false（已入账/已关单/非个人通道）。
func (p *WaitPayPool) CloseAtOf(outTradeNo string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.lookupLocked(outTradeNo); ok {
		return e.CloseAt, true
	}
	return time.Time{}, false
}

// Remove 移出池。幂等：不存在无副作用。
//
// 内部会扫描所有设备/账户桶以定位 outTradeNo（业务上 outTradeNo 全局唯一，正常情况仅匹配一处；
// 扫描全表可防御性清理被异常双写的同名条目，并保证索引不留孤儿）。
func (p *WaitPayPool) Remove(outTradeNo string) {
	if outTradeNo == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	removed := false
	for _, dp := range p.devices {
		for accID, m := range dp.buckets {
			if e, ok := m[outTradeNo]; ok {
				p.unindexLocked(e)
				delete(m, outTradeNo)
				if len(m) == 0 {
					delete(dp.buckets, accID)
					delete(p.acctDev, accID)
				}
				removed = true
			}
		}
	}
	if !removed {
		return
	}
	p.publishGlobalLocked()
}

// Snapshot 返回当前聚合快照。零锁：通过 atomic.Pointer 读取已发布快照。
// 返回的 *Snapshot 与内部 Accounts 切片不可变，调用方可安全并发读取。
func (p *WaitPayPool) Snapshot() *Snapshot {
	return p.snap.Load()
}

// DeviceSnapshot 返回单设备聚合快照（零锁）。设备无子池/无订单 → 空统计（Total=0）。
// 设备调度器与均衡分配器的数据源（PLAN-DYNAMIC-CLOSE-ALLOC §3.2）。
func (p *WaitPayPool) DeviceSnapshot(deviceID string) *DeviceStat {
	p.mu.RLock()
	dp := p.devices[deviceID]
	p.mu.RUnlock()
	if dp == nil {
		return &DeviceStat{DeviceID: deviceID}
	}
	st := dp.stat.Load()
	if st == nil {
		return &DeviceStat{DeviceID: deviceID}
	}
	return st
}

// MatchByNote 备注码精确命中（在账户所属设备子池内匹配）。
// 按 noteCode 找订单，要求 CreatedAt >= since。同码多单时返回 CreatedAt 最新者；未命中返回 nil。
//
// 注意：本方法只做池内匹配，调用方必须另行做 DB 二次校验与账户/通道/空间校验。
func (p *WaitPayPool) MatchByNote(accountID int64, note string, since time.Time) *Entry {
	if note == "" {
		return nil
	}

	p.mu.RLock()
	defer p.mu.RUnlock()

	devID, ok := p.acctDev[accountID]
	if !ok {
		return nil
	}
	dp := p.devices[devID]
	if dp == nil {
		return nil
	}
	noteMap, ok := dp.byNote[accountID]
	if !ok {
		return nil
	}
	outNo, ok := noteMap[note]
	if !ok {
		return nil
	}
	entryMap, ok := dp.buckets[accountID]
	if !ok {
		return nil
	}
	e, ok := entryMap[outNo]
	if !ok {
		return nil // 索引与桶不一致（理论上不应发生；防御性返回 nil）
	}
	if e.CreatedAt.Before(since) {
		return nil
	}
	return e
}

// MatchByAmount 金额兜底候选（在账户所属设备子池内匹配）。
// 按 amountMinor 找订单（CreatedAt >= since），返回全部候选。
// 多候选由调用方判歧义转人工；未命中返回 nil。
func (p *WaitPayPool) MatchByAmount(accountID int64, amountMinor int64, since time.Time) []*Entry {
	p.mu.RLock()
	defer p.mu.RUnlock()

	devID, ok := p.acctDev[accountID]
	if !ok {
		return nil
	}
	dp := p.devices[devID]
	if dp == nil {
		return nil
	}
	amtMap, ok := dp.byAmount[accountID]
	if !ok {
		return nil
	}
	set, ok := amtMap[amountMinor]
	if !ok || len(set) == 0 {
		return nil
	}
	entryMap, ok := dp.buckets[accountID]
	if !ok {
		return nil
	}

	out := make([]*Entry, 0, len(set))
	for outNo := range set {
		e, ok := entryMap[outNo]
		if !ok {
			continue
		}
		if e.CreatedAt.Before(since) {
			continue
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil
	}
	// 稳定排序：先按 CreatedAt 升序，再按 OutTradeNo 字典序，保证测试可重复与歧义判定确定性
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].OutTradeNo < out[j].OutTradeNo
	})
	return out
}

// Count 某账户待支付订单数。无该账户返回 0。
func (p *WaitPayPool) Count(accountID int64) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	devID, ok := p.acctDev[accountID]
	if !ok {
		return 0
	}
	if dp := p.devices[devID]; dp != nil {
		return len(dp.buckets[accountID])
	}
	return 0
}

// HasDupAmount 某账户（同设备同账户）是否仍存在 ≥2 笔相同金额的待支付订单。
// 调度器 execute 反馈判定用（同金额歧义解除 → 提前终止账单页轮询 visit）。
func (p *WaitPayPool) HasDupAmount(accountID int64) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	devID, ok := p.acctDev[accountID]
	if !ok {
		return false
	}
	dp := p.devices[devID]
	if dp == nil {
		return false
	}
	for _, set := range dp.byAmount[accountID] {
		if len(set) >= 2 {
			return true
		}
	}
	return false
}

// Rebuild 全量重建：清空后批量 Add。启动时调用；调用方负责从 DB 加载 WAIT_PAY 订单。
func (p *WaitPayPool) Rebuild(ctx context.Context, orders []*model.Order) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.devices = make(map[string]*devicePool)
	p.acctDev = make(map[int64]string)
	p.devices[""] = newDevicePool()

	for _, o := range orders {
		if o == nil || o.OutTradeNo == "" {
			continue
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				logger.Warn("pool", "Rebuild 中途取消",
					"reason", ctx.Err(),
					"loaded", len(p.acctDev))
				p.publishGlobalLocked()
				return
			default:
			}
		}
		p.indexLocked(p.orderToEntry(o))
	}
	p.publishGlobalLocked()
}

// Reconcile 对账：入参为当前 DB 中全部 WAIT_PAY 订单（调用方查询）。
// 与池内容 diff：DB 有池无（Add 遗漏）→ 补入；池有 DB 无（已终态未移除）→ 移除。
//
// 返回 (added, removed, err)。当前实现无 I/O，err 恒为 nil；保留签名便于未来加 DB 校验。
func (p *WaitPayPool) Reconcile(ctx context.Context, dbOrders []*model.Order) (added, removed int, err error) {
	dbSet := make(map[string]struct{}, len(dbOrders))
	for _, o := range dbOrders {
		if o != nil && o.OutTradeNo != "" {
			dbSet[o.OutTradeNo] = struct{}{}
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 池内 outTradeNo 全集
	poolSet := make(map[string]struct{})
	for _, dp := range p.devices {
		for _, m := range dp.buckets {
			for k := range m {
				poolSet[k] = struct{}{}
			}
		}
	}

	// DB 有池无 → Add
	for _, o := range dbOrders {
		if o == nil || o.OutTradeNo == "" {
			continue
		}
		if _, exists := poolSet[o.OutTradeNo]; !exists {
			p.indexLocked(p.orderToEntry(o))
			added++
		}
	}

	// 池有 DB 无 → Remove
	for outNo := range poolSet {
		if _, exists := dbSet[outNo]; !exists {
			for _, dp := range p.devices {
				for accID, m := range dp.buckets {
					if e, ok := m[outNo]; ok {
						p.unindexLocked(e)
						delete(m, outNo)
						if len(m) == 0 {
							delete(dp.buckets, accID)
							delete(p.acctDev, accID)
						}
						removed++
						break
					}
				}
			}
		}
	}

	if added > 0 || removed > 0 {
		logger.Info("pool", "Reconcile 完成",
			"added", added, "removed", removed, "total", len(dbOrders))
	}

	p.publishGlobalLocked()
	return added, removed, nil
}

// ReconcileLoop 周期对账（后台 goroutine）：每 interval 从 loader 拉取 DB 全部
// WAIT_PAY 订单并与池 diff（DB 有池无→补入；池有 DB 无→移除），防漂移。
// loader 由调用方注入（main.go 包 store 查询），本包不依赖 store 避免循环依赖。
// 退出条件：ctx 取消。
func (p *WaitPayPool) ReconcileLoop(ctx context.Context, interval time.Duration, loader func() ([]*model.Order, error)) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if loader == nil {
			continue
		}
		orders, err := loader()
		if err != nil {
			logger.Warn("pool", "对账加载 DB 待支付订单失败（本周期跳过）", "err", err)
			continue
		}
		if added, removed, rerr := p.Reconcile(ctx, orders); rerr != nil {
			logger.Warn("pool", "对账执行失败（本周期跳过）", "err", rerr)
		} else if added > 0 || removed > 0 {
			logger.Info("pool", "对账同步完成", "added", added, "removed", removed)
		}
	}
}

// --- 内部索引维护（必须持写锁） ---

// lookupLocked 全池按 outTradeNo 定位条目（防御性扫描全部设备/账户桶）。
func (p *WaitPayPool) lookupLocked(outTradeNo string) (*Entry, bool) {
	for _, dp := range p.devices {
		for _, m := range dp.buckets {
			if e, ok := m[outTradeNo]; ok {
				return e, true
			}
		}
	}
	return nil, false
}

// subpoolLocked 获取账户所属设备子池（不存在则创建，更新 acctDev）。
func (p *WaitPayPool) subpoolLocked(deviceID string) *devicePool {
	dp, ok := p.devices[deviceID]
	if !ok {
		dp = newDevicePool()
		p.devices[deviceID] = dp
	}
	return dp
}

// indexLocked 将 entry 写入设备子池三张索引表 + 账户→设备反查。
func (p *WaitPayPool) indexLocked(e *Entry) {
	devID := e.DeviceID
	if devID == "" {
		devID = p.deviceOfEntry(e)
		e.DeviceID = devID
	}
	dp := p.subpoolLocked(devID)

	bm, ok := dp.buckets[e.AccountID]
	if !ok {
		bm = make(map[string]*Entry)
		dp.buckets[e.AccountID] = bm
	}
	bm[e.OutTradeNo] = e
	p.acctDev[e.AccountID] = devID

	if e.NoteCode != "" {
		nm, ok := dp.byNote[e.AccountID]
		if !ok {
			nm = make(map[string]string)
			dp.byNote[e.AccountID] = nm
		}
		nm[e.NoteCode] = e.OutTradeNo
	}

	am, ok := dp.byAmount[e.AccountID]
	if !ok {
		am = make(map[int64]map[string]struct{})
		dp.byAmount[e.AccountID] = am
	}
	set, ok := am[e.AmountMinor]
	if !ok {
		set = make(map[string]struct{})
		am[e.AmountMinor] = set
	}
	set[e.OutTradeNo] = struct{}{}
}

// unindexLocked 从设备子池三张索引表移除 entry 的所有引用。
// 注意：buckets 自身的 map[string]*Entry 由调用方负责 delete 与清理空 accountID 桶。
func (p *WaitPayPool) unindexLocked(e *Entry) {
	dp := p.devices[e.DeviceID]
	if dp == nil {
		return
	}
	if nm, ok := dp.byNote[e.AccountID]; ok {
		if cur, ok := nm[e.NoteCode]; ok && cur == e.OutTradeNo {
			delete(nm, e.NoteCode)
			if len(nm) == 0 {
				delete(dp.byNote, e.AccountID)
			}
		}
	}
	if am, ok := dp.byAmount[e.AccountID]; ok {
		if set, ok := am[e.AmountMinor]; ok {
			delete(set, e.OutTradeNo)
			if len(set) == 0 {
				delete(am, e.AmountMinor)
				if len(am) == 0 {
					delete(dp.byAmount, e.AccountID)
				}
			}
		}
	}
}

// publishGlobalLocked 在写锁内重建并发布：全局快照 + 各设备子池 DeviceStat。
// 调用者必须持有写锁（Lock）。
func (p *WaitPayPool) publishGlobalLocked() {
	now := time.Now()
	all := make([]AccountStat, 0, len(p.acctDev))
	grand := 0

	devIDs := make([]string, 0, len(p.devices))
	for id := range p.devices {
		devIDs = append(devIDs, id)
	}
	sort.Strings(devIDs)

	for _, devID := range devIDs {
		dp := p.devices[devID]
		stats, total := accountStatsOf(dp, now)
		if len(stats) == 0 && total == 0 {
			dp.stat.Store(&DeviceStat{DeviceID: devID, UpdatedAt: now})
			continue
		}
		// 动态窗口（当前设备负载下新增订单的窗口；用于分配器/调度器观测）
		win := p.fallback
		if p.winCalc != nil {
			active, t := 0, 0
			for _, st := range stats {
				active++
				t += st.Count
			}
			if w := p.winCalc(active, t); w > 0 {
				win = w
			}
		}
		dp.stat.Store(&DeviceStat{
			DeviceID:    devID,
			Total:       total,
			Accounts:    stats,
			ActiveAccts: len(stats),
			CloseWindow: win,
			UpdatedAt:   now,
		})
		all = append(all, stats...)
		grand += total
	}

	p.snap.Store(&Snapshot{
		Accounts:  all,
		Total:     grand,
		UpdatedAt: now,
	})
}

// accountStatsOf 计算单个设备子池的账户聚合统计（按 accountID 升序，快照稳定）。
func accountStatsOf(dp *devicePool, now time.Time) ([]AccountStat, int) {
	total := 0
	stats := make([]AccountStat, 0, len(dp.buckets))
	accIDs := make([]int64, 0, len(dp.buckets))
	for id := range dp.buckets {
		accIDs = append(accIDs, id)
	}
	sort.Slice(accIDs, func(i, j int) bool { return accIDs[i] < accIDs[j] })
	for _, accID := range accIDs {
		m := dp.buckets[accID]
		if len(m) == 0 {
			continue
		}
		var (
			oldestCreated time.Time
			minClose      time.Time
			first         = true
		)
		for _, e := range m {
			if first {
				oldestCreated = e.CreatedAt
				minClose = e.CloseAt
				first = false
			} else {
				if e.CreatedAt.Before(oldestCreated) {
					oldestCreated = e.CreatedAt
				}
				if e.CloseAt.Before(minClose) {
					minClose = e.CloseAt
				}
			}
		}
		dupAmount := false
		if am := dp.byAmount[accID]; len(am) > 0 {
			for _, set := range am {
				if len(set) >= 2 { // 同金额 ≥2 笔 → 通知栏匹配歧义，需账本页拆单
					dupAmount = true
					break
				}
			}
		}
		age := now.Sub(oldestCreated)
		if age < 0 {
			age = 0
		}
		ttl := minClose.Sub(now) // 可为负（已超时）
		stats = append(stats, AccountStat{
			AccountID: accID,
			Count:     len(m),
			DupAmount: dupAmount,
			OldestAge: age,
			MinTTL:    ttl,
		})
		total += len(m)
	}
	return stats, total
}
