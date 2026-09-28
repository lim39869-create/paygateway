package ledgermon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"paygateway/internal/model"
	"paygateway/internal/pool"
)

// newTestScheduler 创建测试调度器（无 actor，用假 poller 计数）。
// 测试置 0 poll 间隔（PollMaxGap<=0 退化不等待），避免随机 1~3s 等待拖慢单测。
func newTestScheduler(t *testing.T, p *pool.WaitPayPool, cfg SchedulerConfig) (*DeviceScheduler, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	if cfg.BaseQuota <= 0 {
		cfg = DefaultSchedulerConfig()
	}
	cfg.PollMinGap, cfg.PollMaxGap = 0, 0
	// 池注入设备映射：全部账户归 "devT"（调度器读本设备快照）
	p.SetDeviceOf(func(accountID int64) (string, bool) { return "devT", true })
	s := NewDeviceScheduler("devT", nil, p, cfg)
	s.cfg.AccountPoller = func(ctx context.Context, provider string, userID int) error {
		calls.Add(1)
		return nil
	}
	s.SetAccounts([]schedulerAcct{
		{accountID: 1, provider: "alipay", userID: 0},
		{accountID: 2, provider: "wechat", userID: 0},
		{accountID: 3, provider: "wechat", userID: 999},
	})
	return s, &calls
}

// TestPlan_QuotaProportional 配额与订单数挂钩：订单多的账户分更多配额（floor=1，cap 约束）
func TestPlan_QuotaProportional(t *testing.T) {
	p := pool.NewWaitPayPool(30)
	// 账户1: 10单同金额，账户2: 5单同金额，账户3: 1单 → 仅前两者含同金额订单
	for i := 0; i < 10; i++ {
		p.Add(orderWith(1, "o1_"+itos(i), 100))
	}
	for i := 0; i < 5; i++ {
		p.Add(orderWith(2, "o2_"+itos(i), 200))
	}
	p.Add(orderWith(3, "o3_0", 300))

	s, _ := newTestScheduler(t, p, DefaultSchedulerConfig())
	order, quotas := s.plan(p.DeviceSnapshot("devT"))

	// 账户3 仅 1 单（无同金额）→ 不再入列（仅同金额订单账户才轮询账单页）
	if len(order) != 2 {
		t.Fatalf("仅同金额账户应入列，实际 %d（账户3 无同金额应排除）", len(order))
	}
	// 账户1 订单最多 → 配额应 ≥ 账户2 配额
	if quotas[1] < quotas[2] {
		t.Fatalf("订单多的账户配额应不少于订单少的，acct1=%d acct2=%d", quotas[1], quotas[2])
	}
	// cap 约束：任何账户配额 ≤ Q_total × 60%
	qTotal := 2 * s.cfg.BaseQuota
	capQ := int(float64(qTotal) * s.cfg.CapPct)
	for _, q := range quotas {
		if q > capQ {
			t.Fatalf("配额 %d 超过 cap %d", q, capQ)
		}
	}
}

// TestPlan_OrderByOldestAge 顺序维度：等待最久的账户先做（age 降序）
func TestPlan_OrderByOldestAge(t *testing.T) {
	p := pool.NewWaitPayPool(30)
	now := time.Now()
	// 账户2 的订单最久（提前 10min）且同为金额100；账户1 新近且同为金额100
	p.Add(&model.Order{OutTradeNo: "old", AccountID: 2, AmountMinor: 100, CreatedAt: now.Add(-10 * time.Minute)})
	p.Add(&model.Order{OutTradeNo: "old2", AccountID: 2, AmountMinor: 100, CreatedAt: now.Add(-9 * time.Minute)})
	p.Add(&model.Order{OutTradeNo: "new", AccountID: 1, AmountMinor: 100, CreatedAt: now})
	p.Add(&model.Order{OutTradeNo: "new2", AccountID: 1, AmountMinor: 100, CreatedAt: now})

	s, _ := newTestScheduler(t, p, DefaultSchedulerConfig())
	order, _ := s.plan(p.DeviceSnapshot("devT"))
	if len(order) != 2 {
		t.Fatalf("应 2 个活跃账户（各自有同金额订单），实际 %d", len(order))
	}
	if order[0] != 2 {
		t.Fatalf("等待最久的账户2 应先执行，实际顺序 %v", order)
	}
}

// TestPlan_UrgentPreemptsAll 硬紧急：minTTL < UrgentTTL 的账户无条件置顶（即使订单少）
func TestPlan_UrgentPreemptsAll(t *testing.T) {
	p := pool.NewWaitPayPool(30)
	now := time.Now()
	// 账户1: 8 单同金额（含歧义）；账户2: 2 单同金额但即将关单
	for i := 0; i < 8; i++ {
		p.Add(&model.Order{OutTradeNo: "bulk_" + itos(i), AccountID: 1, AmountMinor: 100, CreatedAt: now.Add(-30 * time.Second)})
	}
	// 需要强制账户2 MinTTL 很小：CloseAt = CreatedAt + 30min 不够紧急。改用 1min 窗口池。
	p2 := pool.NewWaitPayPool(1)
	for i := 0; i < 8; i++ {
		p2.Add(&model.Order{OutTradeNo: "bulk_" + itos(i), AccountID: 1, AmountMinor: 100, CreatedAt: now.Add(-20 * time.Second)})
	}
	p2.Add(&model.Order{OutTradeNo: "urgent", AccountID: 2, AmountMinor: 500, CreatedAt: now.Add(-55 * time.Second)})
	p2.Add(&model.Order{OutTradeNo: "urgent2", AccountID: 2, AmountMinor: 500, CreatedAt: now.Add(-54 * time.Second)})

	s, _ := newTestScheduler(t, p2, DefaultSchedulerConfig())
	order, _ := s.plan(p2.DeviceSnapshot("devT"))
	if len(order) != 2 {
		t.Fatalf("应 2 个活跃账户，实际 %d", len(order))
	}
	if order[0] != 2 {
		t.Fatalf("硬紧急账户2 应无条件置顶（8 单账户让位），实际顺序 %v", order)
	}
}

// TestExecute_DwellMaxCap 连续 poll 上限：单账户配额大时连续执行不超过 DwellMax
func TestExecute_DwellMaxCap(t *testing.T) {
	p := pool.NewWaitPayPool(30)
	// 仅账户1 大量订单 → 窗口内配额可能 > DwellMax，但连续执行被 DwellMax 限制
	for i := 0; i < 20; i++ {
		p.Add(orderWith(1, "o1_"+itos(i), 100))
	}
	s, calls := newTestScheduler(t, p, DefaultSchedulerConfig())
	order, quotas := s.plan(p.DeviceSnapshot("devT"))
	s.execute(tctx(), order, quotas)
	if got := calls.Load(); got > int64(s.cfg.DwellMax) {
		t.Fatalf("单账户连续 poll 超过 DwellMax=%d，实际 %d", s.cfg.DwellMax, got)
	}
}

// TestExecute_EarlyTerminateDupResolved 提前终止：visit 中途同金额歧义解除 → 立即停止该 visit
func TestExecute_EarlyTerminateDupResolved(t *testing.T) {
	p := pool.NewWaitPayPool(30)
	p.Add(orderWith(1, "o1_0", 100))
	p.Add(orderWith(1, "o1_1", 100))
	p.Add(orderWith(2, "o2_0", 500))
	p.Add(orderWith(2, "o2_1", 500))

	s, _ := newTestScheduler(t, p, DefaultSchedulerConfig())
	// 自定义 poller：统计账户1(alipay/user0)的 poll 次数；第一次 poll 后清空账户1 池（模拟同金额订单入账），
	// 验证 visit 提前终止——账户1 的 poll 次数应远小于其配额。
	var acct1Calls atomic.Int64
	s.cfg.AccountPoller = func(ctx context.Context, provider string, userID int) error {
		if provider == "alipay" && userID == 0 {
			n := acct1Calls.Add(1)
			if n == 1 {
				p.Remove("o1_0")
				p.Remove("o1_1")
			}
		}
		return nil
	}
	order, quotas := s.plan(p.DeviceSnapshot("devT"))
	s.execute(tctx(), order, quotas)
	if got := acct1Calls.Load(); got >= int64(quotas[1]) && quotas[1] > 1 {
		t.Fatalf("同金额歧义解除后应提前终止 visit，配额=%d 实际 poll=%d", quotas[1], got)
	}
}

// TestPlan_NoDupNoActive 账户仅有金额互异的订单 → 不触发账单页轮询（通知栏按金额唯一匹配即可）
func TestPlan_NoDupNoActive(t *testing.T) {
	p := pool.NewWaitPayPool(30)
	p.Add(orderWith(1, "o1_0", 100))
	p.Add(orderWith(1, "o1_1", 200))
	p.Add(orderWith(2, "o2_0", 300))

	s, _ := newTestScheduler(t, p, DefaultSchedulerConfig())
	order, quotas := s.plan(p.DeviceSnapshot("devT"))
	if len(order) != 0 || quotas != nil {
		t.Fatalf("无同金额订单应不触发账单页轮询：order=%v quotas=%v", order, quotas)
	}
}

// TestPlan_DupTriggersActive 同一账户出现 ≥2 笔同金额订单 → 触发账单页轮询
func TestPlan_DupTriggersActive(t *testing.T) {
	p := pool.NewWaitPayPool(30)
	// 账户1: 两笔同为100 → 触发；账户2: 一笔100一笔200 → 不触发
	p.Add(orderWith(1, "a1", 100))
	p.Add(orderWith(1, "a2", 100))
	p.Add(orderWith(2, "b1", 100))
	p.Add(orderWith(2, "b2", 200))

	s, _ := newTestScheduler(t, p, DefaultSchedulerConfig())
	order, _ := s.plan(p.DeviceSnapshot("devT"))
	if len(order) != 1 || order[0] != 1 {
		t.Fatalf("仅同金额账户1 应触发，实际 order=%v", order)
	}
}

// TestPlan_NoActiveNoQuota 池空 → 无活跃账户 → 无窗口
func TestPlan_NoActiveNoQuota(t *testing.T) {
	p := pool.NewWaitPayPool(30)
	s, _ := newTestScheduler(t, p, DefaultSchedulerConfig())
	order, quotas := s.plan(p.DeviceSnapshot("devT"))
	if len(order) != 0 || quotas != nil {
		t.Fatalf("池空应无活跃账户：order=%v quotas=%v", order, quotas)
	}
}

// ---- 测试辅助 ----

func pollingScheduler(gapMin, gapMax time.Duration) *DeviceScheduler {
	p := pool.NewWaitPayPool(30)
	s := NewDeviceScheduler("devT", nil, p, SchedulerConfig{})
	s.cfg.PollMinGap, s.cfg.PollMaxGap = gapMin, gapMax
	return s
}

func TestPollGap_Range(t *testing.T) {
	s := pollingScheduler(time.Second, 3*time.Second)
	for i := 0; i < 500; i++ {
		if g := s.pollGap(); g < time.Second || g > 3*time.Second {
			t.Fatalf("pollGap 越界: %v (应∈[1s,3s])", g)
		}
	}
}

func TestPollGap_Disabled(t *testing.T) {
	s := pollingScheduler(0, 0)
	if g := s.pollGap(); g != 0 {
		t.Fatalf("PollMaxGap<=0 应返回 0 不等待，实际 %v", g)
	}
}

func orderWith(accountID int64, outTradeNo string, amountMinor int64) *model.Order {
	return &model.Order{OutTradeNo: outTradeNo, AccountID: accountID, AmountMinor: amountMinor, CreatedAt: time.Now()}
}

func itos(n int) string {
	return string(rune('0' + n))
}

func tctx() context.Context { return context.Background() }
