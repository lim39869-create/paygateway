package main

// 端到端集成测试：完整资金闭环 ——
// 下单(CreateOrder) → 账本监听确认(dispatchLedgerRows) → 入账+账本写入 → 回调 outbox(EnqueueNotify)。
// 验证个人免签通道"从下单到通知上游"全链路在真实装配下自洽（不依赖真实 adb/支付平台）。

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/admin/capture"
	"paygateway/internal/channel"
	"paygateway/internal/ledger"
	"paygateway/internal/model"
	"paygateway/internal/order"
	"paygateway/internal/pool"
	"paygateway/internal/reconciler"
	"paygateway/internal/store"
	"paygateway/internal/upstream"
)

// e2eStubPersonalChannel 端到端测试用个人通道桩：CreatePayment 返回固定备注码，
// 让 CreateOrder 完整走"选账户→分配备注码→生成凭据"路径。
type e2eStubPersonalChannel struct{}

func (e2eStubPersonalChannel) Name() string                           { return "personal.alipay" }
func (e2eStubPersonalChannel) Type() channel.ChannelType              { return channel.TypePersonal }
func (e2eStubPersonalChannel) Kind() channel.ChannelKind              { return channel.KindListener }
func (e2eStubPersonalChannel) Modes() []channel.PayMode               { return []channel.PayMode{channel.ModeQRCode} }
func (e2eStubPersonalChannel) ConfirmAbility() channel.ConfirmAbility { return channel.ConfirmByManual }
func (e2eStubPersonalChannel) Provider() channel.Provider             { return channel.ProviderAlipay }
func (e2eStubPersonalChannel) CreatePayment(_ context.Context, req *channel.PayRequest) (*channel.PayCredential, error) {
	return &channel.PayCredential{
		QRCode:    "https://qr.alipay.com/e2e",
		AccountID: 1, // 测试账户池中的账户（真实通道由 selectServableAccount 决定）
		Mode:      channel.ModeQRCode,
		NoteCode:  "8833", // 固定备注码，账本行携带后精确匹配
		QrToken:   "e2etoken",
		QRContent: "https://qr.alipay.com/e2e",
	}, nil
}
func (e2eStubPersonalChannel) VerifyNotify(context.Context, *http.Request) (*channel.NotifyResult, error) {
	return nil, nil
}
func (e2eStubPersonalChannel) QueryOrder(context.Context, string) (*channel.OrderStatus, error) {
	return nil, order.ErrNotQueryable
}
func (e2eStubPersonalChannel) Listener() channel.EventListener { return nil }

// newE2EEnv 构建端到端测试环境：业务库 + 账本 + 账户池 + 订单服务 + 池 + 通道注册
func newE2EEnv(t *testing.T) (*App, *store.PersonalDB) {
	t.Helper()
	dir := t.TempDir()

	st, err := store.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建业务库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ldb, err := store.NewLedgerDB(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("创建账本库失败: %v", err)
	}
	t.Cleanup(func() { ldb.Close() })

	pdb, err := store.NewPersonalDB(filepath.Join(dir, "personal.db"))
	if err != nil {
		t.Fatalf("创建个人库失败: %v", err)
	}
	t.Cleanup(func() { pdb.Close() })

	if err := account.InitCrypto("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatalf("初始化加密失败: %v", err)
	}

	// 注册 e2e 个人通道（幂等）
	if _, ok := channel.Lookup("personal.alipay"); !ok {
		channel.Register(e2eStubPersonalChannel{})
	}

	accountPool := account.NewPool()
	lg, err := ledger.New(ldb, "")
	if err != nil {
		t.Fatalf("创建账本失败: %v", err)
	}
	svc := order.New(st, lg, accountPool, upstream.NewPool())
	waitPayPool := pool.NewWaitPayPool(30)
	svc.SetWaitPayHooks(&order.WaitPayHooks{
		Add:    waitPayPool.Add,
		Upsert: waitPayPool.Upsert,
		Remove: waitPayPool.Remove,
	})
	// 设备维度 + 动态窗口（与生产装配一致）
	waitPayPool.SetDeviceOf(func(accountID int64) (string, bool) {
		return "dev-e2e", true
	})
	waitPayPool.SetWindowCalc(func(active, total int) time.Duration { return 5 * time.Minute })

	app := &App{
		store:        st,
		personalDB:   pdb,
		accountPool:  accountPool,
		orders:       svc,
		confirmSched: reconciler.NewScheduler(st, svc, nil, nil),
		waitPayPool:  waitPayPool,
	}
	return app, pdb
}

// TestE2E_PersonalOrderFullLifecycle 个人免签完整资金闭环：
// 下单 → 备注码匹配 → 入账(账本写入) → 回调 outbox 生成 → 订单终态 SUCCESS。
func TestE2E_PersonalOrderFullLifecycle(t *testing.T) {
	ctx := context.Background()
	app, pdb := newE2EEnv(t)

	// 账户池：一个可用个人账户（CreateOrder 选账户用）
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "e2e账户", Status: account.StatusNormal,
		QRContent: "https://qr.alipay.com/e2e",
	})
	// 设备 + 绑定账户（监听确认需要设备上下文）
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-e2e")

	// ① 下单（走 CreateOrder 完整路径，stub 通道返回备注码 8833）
	res, err := app.orders.CreateOrder(ctx, &order.CreateRequest{
		OutTradeNo: "E2E-001",
		Type:       "personal.alipay",
		Name:       "测试商品",
		Money:      "10.00",
		NotifyURL:  "http://example.com/notify",
		BuyerKey:   "e2e-buyer-1",
	})
	if err != nil {
		t.Fatalf("下单失败: %v", err)
	}
	if res.Order.Status != model.StatusWaitPay {
		t.Fatalf("下单后应 WAIT_PAY，实际 %s", res.Order.Status)
	}
	if res.Order.NoteCode != "8833" {
		t.Fatalf("订单应携带备注码 8833，实际 %q", res.Order.NoteCode)
	}

	// ② 账本监听：用户已转账 10.00，账本页出现收款行（携带备注码 8833）
	rows := []capture.AlipayLedgerRow{{
		Payer: "*测", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+10.00", AmountMinor: 1000,
		Note: "8833",
	}}
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, rows); err != nil {
		t.Fatalf("dispatchLedgerRows 失败: %v", err)
	}

	// ③ 订单入账 + 账本写入 + 回调 outbox
	o, err := app.store.GetOrder(ctx, "E2E-001")
	if err != nil || o == nil {
		t.Fatalf("查询订单失败: %v", err)
	}
	if o.Status != model.StatusSuccess {
		t.Fatalf("订单应 TRADE_SUCCESS，实际 %s", o.Status)
	}
	if o.PaidAt.IsZero() {
		t.Fatalf("订单应记录 PaidAt")
	}
	if o.DeviceID != dev.DeviceID {
		t.Fatalf("监听确认后订单应回填设备 dev-e2e，实际 %q", o.DeviceID)
	}

	// 回调 outbox 应有通知任务（status=pending）
	pending, err := app.store.CountNotifyTasks(ctx, "pending")
	if err != nil {
		t.Fatalf("CountNotifyTasks: %v", err)
	}
	if pending < 1 {
		t.Fatalf("入账后应生成回调任务, pending=%d", pending)
	}

	// ④ 池内订单应已移除（终态清理）
	if e := app.waitPayPool.MatchByNote(1, "8833", time.Now().Add(-time.Hour)); e != nil {
		t.Fatalf("终态订单应从池中移除, 仍命中: %+v", e)
	}
}

// TestE2E_DuplicateLedgerNoDoubleSettle 重复推送同一收款行 → 幂等不重复入账
func TestE2E_DuplicateLedgerNoDoubleSettle(t *testing.T) {
	ctx := context.Background()
	app, pdb := newE2EEnv(t)

	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "e2e账户", Status: account.StatusNormal,
		QRContent: "https://qr.alipay.com/e2e",
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-e2e-dup")

	if _, err := app.orders.CreateOrder(ctx, &order.CreateRequest{
		OutTradeNo: "E2E-DUP", Type: "personal.alipay", Name: "测试", Money: "5.00",
		NotifyURL: "http://example.com/notify", BuyerKey: "e2e-buyer-dup",
	}); err != nil {
		t.Fatalf("下单失败: %v", err)
	}

	row := capture.AlipayLedgerRow{
		Payer: "*重", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+5.00", AmountMinor: 500, Note: "8833",
	}
	// 第一次推送 → 入账
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, []capture.AlipayLedgerRow{row}); err != nil {
		t.Fatalf("首次 dispatch 失败: %v", err)
	}
	// 第二次推送同一行（模拟轮询重复读取）→ 应去重，不重复入账
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, []capture.AlipayLedgerRow{row}); err != nil {
		t.Fatalf("二次 dispatch 失败: %v", err)
	}

	o, _ := app.store.GetOrder(ctx, "E2E-DUP")
	if o.Status != model.StatusSuccess {
		t.Fatalf("订单应 SUCCESS: %s", o.Status)
	}
	// 重复推送不应重复入账：回调 outbox 对同一订单只有一条待发任务（订单 ID 唯一）
	tasks, err := app.store.GetPendingNotifyTasks(ctx, 10)
	if err != nil {
		t.Fatalf("GetPendingNotifyTasks: %v", err)
	}
	dupCount := 0
	for _, task := range tasks {
		if task.OrderID == o.ID {
			dupCount++
		}
	}
	if dupCount > 1 {
		t.Fatalf("重复推送不应产生重复回调任务, OrderID=%d 任务数=%d", o.ID, dupCount)
	}
}

// TestE2E_NoOrderNoSettle 收款行无匹配订单 → 不产生任何入账/回调
func TestE2E_NoOrderNoSettle(t *testing.T) {
	ctx := context.Background()
	app, pdb := newE2EEnv(t)

	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "e2e账户", Status: account.StatusNormal,
		QRContent: "https://qr.alipay.com/e2e",
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-e2e-none")

	// 无订单的收款行
	row := capture.AlipayLedgerRow{
		Payer: "*无", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+9.99", AmountMinor: 999, Note: "0000",
	}
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, []capture.AlipayLedgerRow{row}); err != nil {
		t.Fatalf("dispatch 失败(应静默不匹配): %v", err)
	}
	// 无订单不应产生回调任务
	pending, _ := app.store.CountNotifyTasks(ctx, "pending")
	if pending != 0 {
		t.Fatalf("无订单不应产生回调任务, 实际 %d", pending)
	}
}
