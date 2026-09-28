package ledgermon

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"paygateway/internal/admin/capture"
)

// fakeReader 测试用读取器：记录被调用的次数
type fakeReader struct {
	calls atomic.Int64
}

func (f *fakeReader) EnsureForeground(ctx context.Context) error { return nil }
func (f *fakeReader) Read(ctx context.Context) ([]capture.AlipayLedgerRow, error) {
	f.calls.Add(1)
	return nil, nil // 无新行
}

// newTestActor 创建设备 actor 并注册测试账户（adb nil：测试只用 PollCmd/WarmupCmd）
func newTestActor(t *testing.T, reader LedgerReader) *DeviceActor {
	t.Helper()
	actor := NewDeviceActor("dev1", "s1", nil, nil)
	actor.AddAccount(&AccountCtx{
		AccountID: 1,
		Provider:  "alipay",
		UserID:    0,
		Reader:    reader,
		Detector:  NewDetector("dev1", 1, nil),
	})
	t.Cleanup(actor.Stop)
	return actor
}

// TestMonitor_OrderDriven 订单驱动：无待支付订单 → 不投递 PollCmd（读取零次）；
// 激活（有订单）→ 轮询；订单终结 → 自动暂停。
func TestMonitor_OrderDriven(t *testing.T) {
	pending := atomic.Bool{}
	reader := &fakeReader{}
	actor := newTestActor(t, reader)
	mon := NewMonitor(1, "dev1", "alipay", "s1", 0, actor,
		func(ctx context.Context, accountID int64) bool { return pending.Load() },
		20*time.Millisecond) // 快间隔加速测试

	mon.Start()
	defer mon.Stop()

	// 无订单：预热 WarmupCmd 会读取一次建基线，之后不应再轮询（保持 1 次）
	time.Sleep(80 * time.Millisecond)
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("无待支付订单时应仅预热读取 1 次（建基线），实际读取 %d 次", got)
	}
	if mon.IsActive() {
		t.Fatal("无订单时应处于暂停态")
	}

	// 下单激活：开始轮询（在预热 1 次基础上递增）
	pending.Store(true)
	mon.Activate()
	deadline := time.Now().Add(2 * time.Second)
	for reader.calls.Load() == 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := reader.calls.Load(); got <= 1 {
		t.Fatal("激活后应开始轮询（读取超过预热基线）")
	}
	if !mon.IsActive() {
		t.Fatal("有订单时应处于轮询态")
	}

	// 订单终结：应自动暂停（允许至多一轮已在途 poll 完成，之后不再增长）
	pending.Store(false)
	before := reader.calls.Load()
	time.Sleep(150 * time.Millisecond) // 多个间隔窗口
	after := reader.calls.Load()
	if after > before+1 {
		t.Fatalf("订单终结后应暂停轮询（允许 ≤1 轮在途），之前 %d 次，之后 %d 次", before, after)
	}
	if mon.IsActive() {
		t.Fatal("订单终结后应回到暂停态")
	}
}

// TestMonitor_ActivateIdempotent 重复激活幂等：已在轮询时再次激活不产生额外循环
func TestMonitor_ActivateIdempotent(t *testing.T) {
	pending := atomic.Bool{}
	pending.Store(true)
	reader := &fakeReader{}
	actor := newTestActor(t, reader)
	mon := NewMonitor(1, "dev1", "alipay", "s1", 0, actor,
		func(ctx context.Context, accountID int64) bool { return pending.Load() },
		10*time.Millisecond)

	mon.Start()
	defer mon.Stop()
	mon.Activate()
	mon.Activate() // 重复激活
	time.Sleep(60 * time.Millisecond)
	if !mon.IsActive() {
		t.Fatal("激活后应处于轮询态")
	}
}

// TestMonitor_WakeThenPauseThenWake 暂停后可再次被唤醒（新订单）
func TestMonitor_WakeThenPauseThenWake(t *testing.T) {
	pending := atomic.Bool{}
	reader := &fakeReader{}
	actor := newTestActor(t, reader)
	mon := NewMonitor(1, "dev1", "alipay", "s1", 0, actor,
		func(ctx context.Context, accountID int64) bool { return pending.Load() },
		20*time.Millisecond)

	mon.Start()
	defer mon.Stop()

	// 第一轮：激活 → 轮询 → 暂停
	pending.Store(true)
	mon.Activate()
	time.Sleep(60 * time.Millisecond)
	pending.Store(false)
	time.Sleep(60 * time.Millisecond) // 等自动暂停

	// 第二轮：再次激活（新订单）
	before := reader.calls.Load()
	pending.Store(true)
	mon.Activate()
	deadline := time.Now().Add(2 * time.Second)
	for reader.calls.Load() <= before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if reader.calls.Load() <= before {
		t.Fatal("暂停后应可再次被唤醒轮询")
	}
}

// TestDeviceActor_PollDedup PollCmd 同账户去重：并发投递多条同账户 PollCmd 不堆积
// （去重只防排队堆积：正在执行的 poll 被 pop 后同账户可再入队，但队列不会因风暴膨胀）。
func TestDeviceActor_PollDedup(t *testing.T) {
	reader := &fakeReader{}
	actor := newTestActor(t, reader)
	for i := 0; i < 10; i++ {
		actor.Enqueue(&PollCmd{AccountID: 1, Provider: "alipay", UserID: 0})
	}
	// 等待全部执行完成
	deadline := time.Now().Add(2 * time.Second)
	for {
		qlen := actor.QueueLen()
		cur := actor.CurrentCmd()
		if qlen == 0 && cur == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("命令未在期限内执行完: queue=%d current=%q", qlen, cur)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := reader.calls.Load(); got > 10 {
		t.Fatalf("去重后执行次数不应超过投递次数，实际 %d 次", got)
	}
	if actor.QueueLen() != 0 {
		t.Fatal("全部执行完后队列应为空")
	}
}

// TestDeviceActor_SerialExecution 串行执行：WarmupCmd 完成后 LockCmd 才执行
// （命令之间无并发，天然原子）。
func TestDeviceActor_SerialExecution(t *testing.T) {
	reader := &fakeReader{}
	actor := newTestActor(t, reader)
	warmupDone := make(chan error, 1)
	actor.Enqueue(&WarmupCmd{AccountID: 1, Provider: "alipay", UserID: 0, Done: warmupDone})
	if err := <-warmupDone; err != nil {
		t.Fatalf("WarmupCmd 执行失败: %v", err)
	}
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("WarmupCmd 应执行 1 次读取（建基线），实际 %d 次", got)
	}
	if actor.CurrentCmd() != "" {
		t.Fatal("命令完成后 actor 应回到空闲态")
	}
}

// TestDeviceActor_DispatchFailureNoLoss 资金安全：Dispatch 失败时行不被提交 seen，
// 下轮重新检测并重试——绝不永久丢单。
func TestDeviceActor_DispatchFailureNoLoss(t *testing.T) {
	// 自定义行构造器：Fingerprint 为时间|金额
	row := func(ts, amt string) capture.AlipayLedgerRow {
		return capture.AlipayLedgerRow{Time: ts, Amount: amt}
	}
	reader := &seqReader{rows: [][]capture.AlipayLedgerRow{
		{row("10:00:01", "1.00")}, // 首次：建基线
		{row("10:00:02", "2.00")}, // 第二次：新行，Dispatch 失败
		{row("10:00:02", "2.00")}, // 第三次：应再次检测到并成功推送
	}}
	dispatchCalls := atomic.Int64{}
	dispatchErr := atomic.Bool{}
	dispatchErr.Store(true) // 先失败
	actor := NewDeviceActor("dev3", "s3", nil, func(ctx context.Context, deviceID string, provider string, space int, rows []capture.AlipayLedgerRow) error {
		dispatchCalls.Add(1)
		if dispatchErr.Load() {
			return fmt.Errorf("模拟推送失败")
		}
		return nil
	})
	acct := &AccountCtx{
		AccountID: 1, Provider: "alipay", UserID: 0,
		Reader:   reader,
		Detector: NewDetector("dev3", 1, nil),
	}
	actor.AddAccount(acct)
	defer actor.Stop()

	// 第一次 poll：建基线，无推送
	actor.Enqueue(&PollCmd{AccountID: 1, Provider: "alipay", UserID: 0})
	waitDone(t, actor, 1)
	if dispatchCalls.Load() != 0 {
		t.Fatalf("首次建基线不应推送，实际推送 %d 次", dispatchCalls.Load())
	}
	// 第二次 poll：新行，Dispatch 失败 → 不提交
	actor.Enqueue(&PollCmd{AccountID: 1, Provider: "alipay", UserID: 0})
	waitDone(t, actor, 2)
	if dispatchCalls.Load() != 1 {
		t.Fatalf("第二次应尝试推送 1 次，实际 %d 次", dispatchCalls.Load())
	}
	// 第三次 poll：应重新检测到同一行并推送成功（不丢单）
	dispatchErr.Store(false)
	actor.Enqueue(&PollCmd{AccountID: 1, Provider: "alipay", UserID: 0})
	waitDone(t, actor, 3)
	if dispatchCalls.Load() != 2 {
		t.Fatalf("Dispatch 失败后下轮应重试推送（共 2 次），实际 %d 次", dispatchCalls.Load())
	}
	// 第四次 poll：已提交，不再推送
	reader.append(nil)
	actor.Enqueue(&PollCmd{AccountID: 1, Provider: "alipay", UserID: 0})
	waitDone(t, actor, 4)
	if dispatchCalls.Load() != 2 {
		t.Fatalf("提交后不应重复推送，实际推送 %d 次", dispatchCalls.Load())
	}
}

// seqReader 按调用次序返回预置行序列（append 与 Read 可能并发，mutex 保护）
type seqReader struct {
	mu   sync.Mutex
	rows [][]capture.AlipayLedgerRow
	idx  int
}

func (r *seqReader) EnsureForeground(ctx context.Context) error { return nil }
func (r *seqReader) Read(ctx context.Context) ([]capture.AlipayLedgerRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.idx >= len(r.rows) {
		return nil, nil
	}
	rows := r.rows[r.idx]
	r.idx++
	return rows, nil
}
func (r *seqReader) append(rows []capture.AlipayLedgerRow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, rows)
}

// waitDone 等待 actor 处理完第 n 次入队
func waitDone(t *testing.T, actor *DeviceActor, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for actor.QueueLen() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // 等当前命令执行完成
}
