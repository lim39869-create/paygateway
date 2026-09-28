// Package reconciler 容灾对账中心：
// ConfirmScheduler（订单确认调度器，主动查单兜底）+ NotifyReaper（dead 通知管理）
//
// 设计要点：
//   - 事件驱动 + timer heap（min-heap），不轮询全表；只有 TTL 到期的订单才被处理
//   - 回调是快路径（立即入账），查单是慢路径（超时未回调 → 主动查单补账）
//   - 关单前必查单：查单失败宁可推迟关单，绝不误关可能已支付的订单
//   - 自动兜底失败（查单连续失败/金额不一致/状态矛盾）→ 进人工异常池，等人工审查
package reconciler

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"paygateway/internal/channel"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/order"
	"paygateway/internal/store"
)

// Config 确认调度器参数（可配置）
type Config struct {
	ConfirmWait      time.Duration // 下单后正常回调等待窗口（默认 60s）
	QueryIntervalMin time.Duration // 查单最小间隔（默认 30s）
	QueryIntervalMax time.Duration // 查单最大间隔（默认 5m）
	QueryBackoff     time.Duration // 查单失败退避基础（默认 1m）
	CloseAfter       time.Duration // 关单时间（默认 30m）
	QueryFailAlert   int           // 查单连续失败进异常池阈值（默认 5）
	TickInterval     time.Duration // 调度 tick（默认 1s）
	BatchSize        int           // 每 tick 最多处理订单数（默认 50）
	Concurrency      int           // 查单并发上限（默认 10）
}

// DefaultConfig 默认参数
func DefaultConfig() *Config {
	return &Config{
		ConfirmWait:      60 * time.Second,
		QueryIntervalMin: 30 * time.Second,
		QueryIntervalMax: 5 * time.Minute,
		QueryBackoff:     time.Minute,
		CloseAfter:       30 * time.Minute,
		QueryFailAlert:   5,
		TickInterval:     time.Second,
		BatchSize:        50,
		Concurrency:      10,
	}
}

// QueryFunc 通道查单（main 注入：遍历通道注册表）
type QueryFunc func(ctx context.Context, outTradeNo string) (*channel.OrderStatus, error)

// PersonalCheckFunc 个人通道订单专用检查（main 注入，nil = 不检查）
// 返回 (offline, reason)：offline=true 表示监听端离线/异常，订单进异常池人工核实
// 用于个人通道关单前的安全确认：监听端掉线时不能关单（用户可能已付款但未监听到）
type PersonalCheckFunc func(ctx context.Context, o *model.Order) (offline bool, reason string)

// entry 堆元素
type entry struct {
	outTradeNo  string
	nextCheckAt time.Time
	index       int
}

type confirmHeap []*entry

func (h confirmHeap) Len() int           { return len(h) }
func (h confirmHeap) Less(i, j int) bool { return h[i].nextCheckAt.Before(h[j].nextCheckAt) }
func (h confirmHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *confirmHeap) Push(x any) {
	e := x.(*entry)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *confirmHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	e.index = -1
	*h = old[:n-1]
	return e
}

// Scheduler 订单确认调度器
type Scheduler struct {
	store store.Store
	svc   *order.Service
	query QueryFunc
	cfg   *Config

	personalCheck PersonalCheckFunc // 个人通道监听端检查（nil = 不检查，关单前直接关）

	mu         sync.Mutex
	heap       confirmHeap
	idx        map[string]*entry
	checkCount map[string]int // 每单确认次数（退避/进池判定）
}

// NewScheduler 创建确认调度器
func NewScheduler(st store.Store, svc *order.Service, q QueryFunc, cfg *Config) *Scheduler {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	return &Scheduler{
		store:      st,
		svc:        svc,
		query:      q,
		cfg:        cfg,
		heap:       confirmHeap{},
		idx:        make(map[string]*entry),
		checkCount: make(map[string]int),
	}
}

// Query 通道查单（管理后台手动查单/人工复核用）
func (s *Scheduler) Query(ctx context.Context, outTradeNo string) (*channel.OrderStatus, error) {
	if s.query == nil {
		return nil, fmt.Errorf("查单函数未注入")
	}
	return s.query(ctx, outTradeNo)
}

// SetPersonalCheck 注入个人通道监听端检查（ConfirmByListen/ConfirmByManual 通道关单前调用）
func (s *Scheduler) SetPersonalCheck(fn PersonalCheckFunc) {
	s.personalCheck = fn
}

// abilityFor 按订单通道名获取确认能力（未知通道默认按可查单处理）
func abilityFor(chName string) channel.ConfirmAbility {
	ch, ok := channel.Lookup(chName)
	if !ok {
		return channel.ConfirmByQuery
	}
	return ch.ConfirmAbility()
}

// OnOrderCreated 订单创建事件：进入待确认（回调窗口后检查）
func (s *Scheduler) OnOrderCreated(o *model.Order) {
	s.push(o.OutTradeNo, time.Now().Add(s.cfg.ConfirmWait))
}

// OnSettled 订单已入账：移出待确认
func (s *Scheduler) OnSettled(outTradeNo string) { s.remove(outTradeNo) }

// OnClosed 订单已关闭：移出待确认
func (s *Scheduler) OnClosed(outTradeNo string) { s.remove(outTradeNo) }

// Start 启动（阻塞，放 goroutine）：恢复堆 + 周期 tick
func (s *Scheduler) Start(ctx context.Context) {
	s.restore(ctx)
	logger.Info("reconciler", "确认调度器启动", "interval", s.cfg.TickInterval.String(),
		"confirm_wait", s.cfg.ConfirmWait.String(), "close_after", s.cfg.CloseAfter.String())
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("reconciler", "确认调度器停止")
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// restore 启动恢复：加载所有未完成订单重建堆（动作幂等，重复执行无副作用）
func (s *Scheduler) restore(ctx context.Context) {
	orders, err := s.store.ListExpiredOrders(ctx, time.Now())
	if err != nil {
		logger.Error("reconciler", "恢复待确认订单失败", "err", err)
		return
	}
	now := time.Now()
	for _, o := range orders {
		next := o.CreatedAt.Add(s.cfg.ConfirmWait)
		if next.Before(now) {
			next = now // 已过窗口，立即检查
		}
		s.push(o.OutTradeNo, next)
	}
	logger.Info("reconciler", "确认调度器恢复完成", "pending", len(orders))
}

// tick 处理一批到期订单
func (s *Scheduler) tick(ctx context.Context) {
	due := s.popDue(time.Now(), s.cfg.BatchSize)
	if len(due) == 0 {
		return
	}
	sem := make(chan struct{}, s.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, no := range due {
		wg.Add(1)
		sem <- struct{}{}
		go func(outTradeNo string) {
			defer wg.Done()
			defer func() { <-sem }()
			s.confirm(ctx, outTradeNo)
		}(no)
	}
	wg.Wait()
}

// confirm 单个订单的确认流程（查单补账 / 关单 / 进异常池）
func (s *Scheduler) confirm(ctx context.Context, outTradeNo string) {
	o, err := s.store.GetOrder(ctx, outTradeNo)
	if err != nil || o == nil {
		s.remove(outTradeNo)
		return
	}
	// 终态直接出堆（回调可能恰好先到）
	if o.Status == model.StatusSuccess || o.Status == model.StatusClosed {
		s.remove(outTradeNo)
		return
	}
	if o.Status != model.StatusWaitPay {
		s.remove(outTradeNo)
		return
	}

	n := s.incCheck(outTradeNo)

	// 按通道确认能力分叉：个人通道（无法主动查单）走监听确认路径
	if abilityFor(o.Channel) != channel.ConfirmByQuery {
		s.confirmPersonal(ctx, o, n)
		return
	}

	// 已到关单时间：关单前最后查一次（绝不误关已支付订单）
	if time.Since(o.CreatedAt) >= s.cfg.CloseAfter {
		s.confirmBeforeClose(ctx, o, n)
		return
	}

	// 未到关单时间：主动查单
	st, qerr := s.query(ctx, outTradeNo)
	if qerr != nil || st == nil {
		if n >= s.cfg.QueryFailAlert {
			s.svc.EnqueueException(ctx, o, model.ExReasonQueryFailed, "QUERY_FAILED", "",
				s.evidence("query_failed", qerr), "查单连续失败，需人工核对实际支付情况")
			s.remove(outTradeNo)
			return
		}
		s.repush(outTradeNo, s.backoff(n))
		return
	}

	switch st.Status {
	case model.StatusSuccess:
		s.settle(ctx, o, st)
	case model.StatusClosed:
		if ok, _ := s.svc.CloseOrder(ctx, outTradeNo, "query_closed"); ok {
			s.remove(outTradeNo)
		} else {
			s.repush(outTradeNo, 30*time.Second)
		}
	default: // 未支付：自适应间隔复查
		s.repush(outTradeNo, s.queryInterval(n))
	}
}

// confirmBeforeClose 关单前查单（查单失败 → 进异常池，绝不关单）
func (s *Scheduler) confirmBeforeClose(ctx context.Context, o *model.Order, n int) {
	st, qerr := s.query(ctx, o.OutTradeNo)
	if qerr != nil || st == nil {
		s.svc.EnqueueException(ctx, o, model.ExReasonCloseUncertain, "QUERY_FAILED", "",
			s.evidence("close_query_failed", qerr), "关单前查单失败，需人工确认是否已支付")
		s.remove(o.OutTradeNo)
		return
	}
	switch st.Status {
	case model.StatusSuccess:
		s.settle(ctx, o, st)
	case model.StatusClosed:
		if ok, _ := s.svc.CloseOrder(ctx, o.OutTradeNo, "query_before_close"); ok {
			s.remove(o.OutTradeNo)
		}
	default:
		if ok, _ := s.svc.CloseOrder(ctx, o.OutTradeNo, "query_before_close_not_paid"); ok {
			s.remove(o.OutTradeNo)
		}
	}
}

// confirmPersonal 个人通道订单确认（无法主动查单，靠监听端推送入账）
//
// 分叉逻辑：
//   - 未到关单时间：不查单（个人码无查询 API），自适应间隔复查，等监听端推送
//   - 已到关单时间：
//   - 若注入 personalCheck 且监听端离线 → 不能关单（用户可能已付款但未监听到）→ 进异常池人工核实
//   - 监听端在线但无推送 → 用户大概率未付款 → 关单
//   - 复查次数超阈值仍无结果 → 进异常池（人工确认实际收款情况）
func (s *Scheduler) confirmPersonal(ctx context.Context, o *model.Order, n int) {
	// 未到关单时间：等监听端推送（不查单），自适应间隔复查
	if time.Since(o.CreatedAt) < s.cfg.CloseAfter {
		s.repush(o.OutTradeNo, s.queryInterval(n))
		return
	}

	// 已到关单时间
	// 监听端离线检查：离线时订单进异常池（避免误关已付款订单），绝不直接关单
	if s.personalCheck != nil {
		offline, reason := s.personalCheck(ctx, o)
		if offline {
			s.svc.EnqueueException(ctx, o, model.ExReasonListenOffline, "LISTEN_OFFLINE", "",
				s.evidence("listen_offline", nil), "监听端掉线，无法确认用户是否已付款，需人工核实： "+reason)
			s.remove(o.OutTradeNo)
			return
		}
	}

	// 复查次数过多（监听端在线但长时间无推送）：存疑，进异常池人工确认
	if n >= s.cfg.QueryFailAlert {
		s.svc.EnqueueException(ctx, o, model.ExReasonListenTimeout, "LISTEN_TIMEOUT", "",
			s.evidence("listen_timeout", nil), "监听端在线但长时间未收到收款推送，需人工确认实际收款情况")
		s.remove(o.OutTradeNo)
		return
	}

	// 监听端在线 + 无推送 + 未超复查阈值：关单（用户大概率未付款）
	if ok, _ := s.svc.CloseOrder(ctx, o.OutTradeNo, "personal_timeout_close"); ok {
		s.remove(o.OutTradeNo)
	} else {
		s.repush(o.OutTradeNo, 30*time.Second)
	}
}

// settle 查单补账（金额不一致自动进异常池）
func (s *Scheduler) settle(ctx context.Context, o *model.Order, st *channel.OrderStatus) {
	ok, err := s.svc.SettleByQuery(ctx, o.OutTradeNo, st.ChannelTxID, st.AmountMinor,
		s.evidence("query_success", nil))
	if err != nil {
		// 金额不一致已自动进池；其他错误重试
		if !strings.Contains(err.Error(), "金额") {
			s.repush(o.OutTradeNo, s.cfg.QueryBackoff)
		} else {
			s.remove(o.OutTradeNo)
		}
		return
	}
	if ok {
		logger.Info("reconciler", "查单补账成功", "out_trade_no", o.OutTradeNo, "tx_id", st.ChannelTxID)
		s.remove(o.OutTradeNo)
	} else {
		s.repush(o.OutTradeNo, 30*time.Second)
	}
}

// ========== heap 操作 ==========

func (s *Scheduler) push(outTradeNo string, next time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.idx[outTradeNo]; ok {
		e.nextCheckAt = next
		heap.Fix(&s.heap, e.index)
		return
	}
	e := &entry{outTradeNo: outTradeNo, nextCheckAt: next}
	s.idx[outTradeNo] = e
	heap.Push(&s.heap, e)
}

func (s *Scheduler) remove(outTradeNo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.idx[outTradeNo]; ok {
		heap.Remove(&s.heap, e.index)
		delete(s.idx, outTradeNo)
	}
	delete(s.checkCount, outTradeNo)
}

func (s *Scheduler) repush(outTradeNo string, delay time.Duration) {
	s.push(outTradeNo, time.Now().Add(delay))
}

// popDue 取出到期且数量受限的订单
func (s *Scheduler) popDue(now time.Time, max int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for len(s.heap) > 0 && len(out) < max {
		top := s.heap[0]
		if top.nextCheckAt.After(now) {
			break
		}
		heap.Pop(&s.heap)
		delete(s.idx, top.outTradeNo)
		out = append(out, top.outTradeNo)
	}
	return out
}

func (s *Scheduler) incCheck(outTradeNo string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkCount[outTradeNo]++
	return s.checkCount[outTradeNo]
}

// backoff 查单失败退避：1m/5m/15m/1h → 上限 1h
func (s *Scheduler) backoff(n int) time.Duration {
	steps := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	if n-1 < len(steps) {
		return steps[n-1]
	}
	return time.Hour
}

// queryInterval 未支付复查间隔：30s → 1m → 2m → 5m（自适应拉长）
func (s *Scheduler) queryInterval(n int) time.Duration {
	iv := s.cfg.QueryIntervalMin
	for i := 1; i < n && iv < s.cfg.QueryIntervalMax; i++ {
		iv *= 2
	}
	if iv > s.cfg.QueryIntervalMax {
		iv = s.cfg.QueryIntervalMax
	}
	return iv
}

// evidence 生成证据时间线片段（JSON，自动收集只读）
func (s *Scheduler) evidence(action string, err error) string {
	m := map[string]any{"action": action, "time": time.Now().Format(time.RFC3339)}
	if err != nil {
		m["error"] = err.Error()
	}
	b, _ := json.Marshal(m)
	return string(b)
}
