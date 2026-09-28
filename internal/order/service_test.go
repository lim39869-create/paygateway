package order

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/channel"
	"paygateway/internal/ledger"
	"paygateway/internal/model"
	"paygateway/internal/store"
	"paygateway/internal/upstream"
)

// newTestService 创建真实 SQLite + 真实账本的测试服务
func newTestService(t *testing.T) (*Service, *store.SQLiteStore, *store.LedgerDB, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.NewSQLite(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("创建 SQLite 失败: %v", err)
	}
	lgdb, err := store.NewLedgerDB(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("创建账本 DB 失败: %v", err)
	}
	lg, err := ledger.New(lgdb, "") // 自动生成测试密钥
	if err != nil {
		t.Fatalf("创建账本失败: %v", err)
	}
	svc := New(st, lg, account.NewPool(), upstream.NewPool())
	return svc, st, lgdb, dir
}

// createTestOrder 直接落库一笔待支付订单（绕过通道注册，专注测资金路径）
func createTestOrder(t *testing.T, st *store.SQLiteStore, outTradeNo string, amount int64) *model.Order {
	t.Helper()
	now := time.Now()
	o := &model.Order{
		OutTradeNo:  outTradeNo,
		GatewayNo:   "GW" + outTradeNo,
		Channel:     "alipay",
		Pid:         "2001",
		AmountMinor: amount,
		Currency:    "CNY",
		Subject:     "测试",
		Status:      model.StatusWaitPay,
		NotifyURL:   "http://example.com/notify",
		CreatedAt:   now,
	}
	if err := st.CreateOrder(context.Background(), o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	return o
}

// TestSettlePayment_Normal 正常结算：状态→SUCCESS，事件+账本都有记录
func TestSettlePayment_Normal(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	o := createTestOrder(t, st, "NORMAL001", 100) // 1.00 元

	ok, err := svc.SettlePayment(ctx, &channel.NotifyResult{
		OutTradeNo:  o.OutTradeNo,
		ChannelTxID: "ALI_TX_001",
		AmountMinor: 100,
	})
	if err != nil || !ok {
		t.Fatalf("SettlePayment = %v, %v; 期望 true, nil", ok, err)
	}

	// 状态
	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	if got.Status != model.StatusSuccess {
		t.Fatalf("订单状态 = %s, 期望 %s", got.Status, model.StatusSuccess)
	}
	// 账本有记录
	has, err := svc.ledger.HasEvent(ctx, got.ID)
	if err != nil || !has {
		t.Fatalf("账本应有记录, has=%v err=%v", has, err)
	}
	// 事件有记录
	evs, _ := st.ListEvents(ctx, got.ID, 10)
	if len(evs) != 1 || evs[0].EventType != "paid" {
		t.Fatalf("事件记录 = %+v, 期望 1 条 paid", evs)
	}
}

// TestSettlePayment_Idempotent 幂等：重复回调不重复记账
func TestSettlePayment_Idempotent(t *testing.T) {
	svc, st, lgdb, _ := newTestService(t)
	ctx := context.Background()
	o := createTestOrder(t, st, "IDEM001", 100)

	// 第一次结算
	if ok, err := svc.SettlePayment(ctx, &channel.NotifyResult{OutTradeNo: o.OutTradeNo, ChannelTxID: "ALI_TX_2", AmountMinor: 100}); err != nil || !ok {
		t.Fatalf("第一次结算失败: %v %v", ok, err)
	}
	count1, _ := lgdb.CountLedger(ctx)

	// 重复回调（同交易号）
	if ok, err := svc.SettlePayment(ctx, &channel.NotifyResult{OutTradeNo: o.OutTradeNo, ChannelTxID: "ALI_TX_2", AmountMinor: 100}); err != nil || !ok {
		t.Fatalf("幂等重试失败: %v %v", ok, err)
	}
	count2, _ := lgdb.CountLedger(ctx)
	if count2 != count1 {
		t.Fatalf("幂等重试不应重复记账: before=%d after=%d", count1, count2)
	}
	// 事件也应只有 1 条 paid
	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	evs, _ := st.ListEvents(ctx, got.ID, 10)
	paid := 0
	for _, e := range evs {
		if e.EventType == "paid" {
			paid++
		}
	}
	if paid != 1 {
		t.Fatalf("paid 事件条数 = %d, 期望 1", paid)
	}
}

// TestSettlePayment_CrashRecovery 崩溃恢复：状态已 SUCCESS 但账本缺失（模拟条件更新后崩溃）→ 幂等分支补写
func TestSettlePayment_CrashRecovery(t *testing.T) {
	svc, st, lgdb, _ := newTestService(t)
	ctx := context.Background()
	o := createTestOrder(t, st, "CRASH001", 100)

	// 模拟崩溃窗口：仅条件更新状态为 SUCCESS（等价于进程在事件/账本写入前崩溃）
	ok, err := st.UpdateOrderStatus(ctx, o.OutTradeNo, model.StatusWaitPay, model.StatusSuccess, nil)
	if err != nil || !ok {
		t.Fatalf("模拟崩溃状态更新失败: %v %v", ok, err)
	}
	// 确认账本确实没有记录
	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	has, _ := svc.ledger.HasEvent(ctx, got.ID)
	if has {
		t.Fatal("前置条件错误：账本不应有记录")
	}

	// 回调重试 → 幂等分支 → ensureRecorded 补写账本
	if ok, err := svc.SettlePayment(ctx, &channel.NotifyResult{OutTradeNo: o.OutTradeNo, ChannelTxID: "ALI_TX_3", AmountMinor: 100}); err != nil || !ok {
		t.Fatalf("崩溃恢复结算失败: %v %v", ok, err)
	}
	has, err = svc.ledger.HasEvent(ctx, got.ID)
	if err != nil || !has {
		t.Fatalf("崩溃恢复后账本应有记录, has=%v err=%v", has, err)
	}
	// 不应重复记账（再触发一次幂等返回，账本条数不变）
	count1, _ := lgdb.CountLedger(ctx)
	svc.SettlePayment(ctx, &channel.NotifyResult{OutTradeNo: o.OutTradeNo, ChannelTxID: "ALI_TX_3", AmountMinor: 100})
	count2, _ := lgdb.CountLedger(ctx)
	if count2 != count1 {
		t.Fatalf("崩溃恢复后再结算不应重复记账: before=%d after=%d", count1, count2)
	}
}

// TestSettlePayment_AmountMismatch 金额不匹配：拒绝入账（防改价）
func TestSettlePayment_AmountMismatch(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	o := createTestOrder(t, st, "AMT001", 100)

	ok, err := svc.SettlePayment(ctx, &channel.NotifyResult{
		OutTradeNo:  o.OutTradeNo,
		ChannelTxID: "ALI_TX_4",
		AmountMinor: 50, // 实付 0.50 ≠ 订单 1.00
	})
	if err == nil || ok {
		t.Fatalf("金额不匹配应返回错误并拒绝, ok=%v err=%v", ok, err)
	}
	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	if got.Status == model.StatusSuccess {
		t.Fatal("金额不匹配时订单不应变为 SUCCESS")
	}
	// 应记录 amount_mismatch 事件（审计）
	evs, _ := st.ListEvents(ctx, got.ID, 10)
	found := false
	for _, e := range evs {
		if e.EventType == "amount_mismatch" {
			found = true
		}
	}
	if !found {
		t.Fatal("应记录 amount_mismatch 审计事件")
	}
}

// TestSettleManual_Recovery 人工入账 + 幂等不重复记账
func TestSettleManual_Recovery(t *testing.T) {
	svc, st, lgdb, _ := newTestService(t)
	ctx := context.Background()
	o := createTestOrder(t, st, "MANUAL001", 100)

	ok, err := svc.SettleManual(ctx, o.OutTradeNo, "MAN_TX_1", 100, "payer1", "admin", "测试")
	if err != nil || !ok {
		t.Fatalf("人工入账失败: %v %v", ok, err)
	}
	count1, _ := lgdb.CountLedger(ctx)

	// 幂等重试不重复记账
	if ok, err := svc.SettleManual(ctx, o.OutTradeNo, "MAN_TX_1", 100, "payer1", "admin", "重试"); err != nil || !ok {
		t.Fatalf("人工入账幂等重试失败: %v %v", ok, err)
	}
	count2, _ := lgdb.CountLedger(ctx)
	if count2 != count1 {
		t.Fatalf("人工入账幂等重试不应重复记账: before=%d after=%d", count1, count2)
	}
}

// TestSettlePayment_DeviceID 个人免签：SettlePayment 带 device_id → 订单 + 账本正确记录
func TestSettlePayment_DeviceID(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	o := createTestOrder(t, st, "DEVICE001", 100)
	deviceID := "test-device-12345"

	// 下单时绑定收款账户（CreateOrder 路径语义：cred.AccountID → 订单 AccountID）
	if ok, err := st.UpdateOrderStatus(ctx, o.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		upd.AccountID = 2
	}); err != nil || !ok {
		t.Fatalf("设置订单账户失败: %v %v", ok, err)
	}

	ok, err := svc.SettlePayment(ctx, &channel.NotifyResult{
		OutTradeNo:  o.OutTradeNo,
		ChannelTxID: "PSN-DEVICE-001",
		AmountMinor: 100,
		Payer:       "personal:" + deviceID,
		DeviceID:    deviceID,
		AccountID:   2,
	})
	if err != nil || !ok {
		t.Fatalf("SettlePayment = %v, %v; 期望 true, nil", ok, err)
	}

	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	if got.DeviceID != deviceID {
		t.Fatalf("订单 device_id = %q, 期望 %q", got.DeviceID, deviceID)
	}
	if got.Payer != "personal:"+deviceID {
		t.Fatalf("订单 payer = %q, 期望 personal:%s", got.Payer, deviceID)
	}
	if got.AccountID != 2 {
		t.Fatalf("订单 account_id = %d, 期望 2", got.AccountID)
	}

	has, err := svc.ledger.HasEvent(ctx, got.ID)
	if err != nil || !has {
		t.Fatalf("账本应有记录, has=%v err=%v", has, err)
	}
	res, err := svc.ledger.Verify(ctx)
	if err != nil || res == nil || !res.Valid {
		t.Fatalf("账本验证应通过, res=%v err=%v", res, err)
	}
}

// TestCloseExpiredOrders_PersonalOffline_NoClose 漏单防护：个人通道设备离线时绝不关单
// （CloseExpiredOrders 走安全检查 → 离线 → 进异常池，订单保持 WAIT_PAY）
func TestCloseExpiredOrders_PersonalOffline_NoClose(t *testing.T) {
	registerStubPersonal(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	// 个人通道订单（旧于关单窗口）
	now := time.Now()
	o := &model.Order{
		OutTradeNo:  "P_OFFLINE",
		GatewayNo:   "GW_P_OFFLINE",
		Channel:     "personal.alipay",
		Pid:         "2001",
		AmountMinor: 1000,
		Currency:    "CNY",
		Subject:     "测试",
		Status:      model.StatusWaitPay,
		NotifyURL:   "http://example.com/notify",
		CreatedAt:   now.Add(-40 * time.Minute),
		DeviceID:    "dev-offline",
	}
	if err := st.CreateOrder(ctx, o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	// 注入 personalCheck：模拟设备离线（返回 offline=true）
	svc.SetPersonalCheck(func(ctx context.Context, o *model.Order) (bool, string) {
		return true, "设备 adb 不可达（测试模拟）"
	})

	closed, err := svc.CloseExpiredOrders(ctx, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("CloseExpiredOrders err: %v", err)
	}
	if closed != 0 {
		t.Fatalf("设备离线时不应关单：closed=%d", closed)
	}

	// 订单必须保持 WAIT_PAY（未被误关）
	got, err := st.GetOrder(ctx, o.OutTradeNo)
	if err != nil || got == nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.Status != model.StatusWaitPay {
		t.Fatalf("设备离线时订单应保持 WAIT_PAY，实际 %s（漏单！）", got.Status)
	}

	// 必须已进人工异常池（人工核实路径）
	exc, err := st.GetExceptionByOutTradeNo(ctx, o.OutTradeNo)
	if err != nil || exc == nil {
		t.Fatalf("离线订单应进人工异常池, exc=%v err=%v", exc, err)
	}
	if exc.Reason != model.ExReasonListenOffline {
		t.Fatalf("异常池原因应为 listen_offline: %s", exc.Reason)
	}
}

// TestCloseExpiredOrders_PersonalOnline_Close 个人通道设备在线且已到关单窗口 → 正常关单
func TestCloseExpiredOrders_PersonalOnline_Close(t *testing.T) {
	registerStubPersonal(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	now := time.Now()
	o := &model.Order{
		OutTradeNo:  "P_ONLINE",
		GatewayNo:   "GW_P_ONLINE",
		Channel:     "personal.alipay",
		Pid:         "2001",
		AmountMinor: 1000,
		Currency:    "CNY",
		Subject:     "测试",
		Status:      model.StatusWaitPay,
		NotifyURL:   "http://example.com/notify",
		CreatedAt:   now.Add(-40 * time.Minute),
		DeviceID:    "dev-online",
	}
	if err := st.CreateOrder(ctx, o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	// 设备在线（返回 offline=false）
	svc.SetPersonalCheck(func(ctx context.Context, o *model.Order) (bool, string) {
		return false, ""
	})

	closed, err := svc.CloseExpiredOrders(ctx, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("CloseExpiredOrders err: %v", err)
	}
	if closed != 1 {
		t.Fatalf("设备在线应关单 1 笔：closed=%d", closed)
	}
	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	if got.Status != model.StatusClosed {
		t.Fatalf("设备在线应已关单：%s", got.Status)
	}
}

// TestCloseExpiredOrders_DynamicWindow_NotDue 动态关单窗口：CloseAtOf 未到期 → 跳过不关
func TestCloseExpiredOrders_DynamicWindow_NotDue(t *testing.T) {
	registerStubPersonal(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	now := time.Now()
	o := &model.Order{
		OutTradeNo:  "P_DYN",
		GatewayNo:   "GW_P_DYN",
		Channel:     "personal.alipay",
		Pid:         "2001",
		AmountMinor: 1000,
		Currency:    "CNY",
		Subject:     "测试",
		Status:      model.StatusWaitPay,
		NotifyURL:   "http://example.com/notify",
		CreatedAt:   now.Add(-40 * time.Minute), // 旧订单
	}
	if err := st.CreateOrder(ctx, o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	// 注入 CloseAtOf：模拟动态窗口未到期（CloseAt 在未来）→ 跳过
	svc.SetWaitPayHooks(&WaitPayHooks{
		CloseAtOf: func(outTradeNo string) (time.Time, bool) {
			if outTradeNo == "P_DYN" {
				return now.Add(10 * time.Minute), true // 未到期
			}
			return time.Time{}, false
		},
	})
	// 设备在线（即使在线也不该关，因为窗口未到）
	svc.SetPersonalCheck(func(ctx context.Context, o *model.Order) (bool, string) {
		return false, ""
	})

	closed, err := svc.CloseExpiredOrders(ctx, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("CloseExpiredOrders err: %v", err)
	}
	if closed != 0 {
		t.Fatalf("动态窗口未到期不应关单：closed=%d", closed)
	}
	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	if got.Status != model.StatusWaitPay {
		t.Fatalf("未到期订单应保持 WAIT_PAY：%s", got.Status)
	}
}

// stubPersonalChannel 个人通道桩（测试用）：注册表 Lookup 返回 ConfirmByManual，
// 让 CloseExpiredOrders 走个人安全检查分支（不实际下单/查单）。
type stubPersonalChannel struct{}

func (stubPersonalChannel) Name() string                           { return "personal.alipay" }
func (stubPersonalChannel) Type() channel.ChannelType              { return channel.TypePersonal }
func (stubPersonalChannel) Kind() channel.ChannelKind              { return channel.KindListener }
func (stubPersonalChannel) Modes() []channel.PayMode               { return []channel.PayMode{channel.ModeQRCode} }
func (stubPersonalChannel) ConfirmAbility() channel.ConfirmAbility { return channel.ConfirmByManual }
func (stubPersonalChannel) Provider() channel.Provider             { return channel.ProviderAlipay }
func (stubPersonalChannel) CreatePayment(context.Context, *channel.PayRequest) (*channel.PayCredential, error) {
	return nil, nil
}
func (stubPersonalChannel) VerifyNotify(context.Context, *http.Request) (*channel.NotifyResult, error) {
	return nil, nil
}
func (stubPersonalChannel) QueryOrder(context.Context, string) (*channel.OrderStatus, error) {
	return nil, ErrNotQueryable
}
func (stubPersonalChannel) Listener() channel.EventListener { return nil }

// registerStubPersonal 注册个人通道桩（幂等：已注册跳过）
func registerStubPersonal(t *testing.T) {
	t.Helper()
	if _, ok := channel.Lookup("personal.alipay"); ok {
		return
	}
	channel.Register(stubPersonalChannel{})
}

// TestCloseExpiredOrders_Merchant_RespectsFixedWindow 商家通道固定关单窗口：
// 候选集被动态窗口放宽（before=now-11min）时，未到 ORDER_CLOSE_MINUTES(30min) 的商家订单不关单。
func TestCloseExpiredOrders_Merchant_RespectsFixedWindow(t *testing.T) {
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	now := time.Now()

	// 商家通道订单，20 分钟前创建（早于动态候选集 11min，但未到商家固定 30min）
	o := &model.Order{
		OutTradeNo:  "M_FIXED",
		GatewayNo:   "GW_M_FIXED",
		Channel:     "alipay", // 商家通道（ConfirmByQuery）
		Pid:         "2001",
		AmountMinor: 1000,
		Currency:    "CNY",
		Subject:     "测试",
		Status:      model.StatusWaitPay,
		NotifyURL:   "http://example.com/notify",
		CreatedAt:   now.Add(-20 * time.Minute),
	}
	if err := st.CreateOrder(ctx, o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	// 商家固定关单窗口 30min
	svc.SetCloseWindow(30)

	// 候选集放宽到 11min（模拟动态窗口模式下扫描器传的 before）
	closed, err := svc.CloseExpiredOrders(ctx, now.Add(-11*time.Minute))
	if err != nil {
		t.Fatalf("CloseExpiredOrders err: %v", err)
	}
	if closed != 0 {
		t.Fatalf("商家订单未到 30min 固定窗口不应关单：closed=%d", closed)
	}
	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	if got.Status != model.StatusWaitPay {
		t.Fatalf("商家订单应保持 WAIT_PAY：%s", got.Status)
	}
}

// TestCloseExpiredOrders_Merchant_OverFixedWindow 商家通道已到固定窗口 → 正常关单
func TestCloseExpiredOrders_Merchant_OverFixedWindow(t *testing.T) {
	registerStubMerchant(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	now := time.Now()

	o := &model.Order{
		OutTradeNo:  "M_OVER",
		GatewayNo:   "GW_M_OVER",
		Channel:     "alipay",
		Pid:         "2001",
		AmountMinor: 1000,
		Currency:    "CNY",
		Subject:     "测试",
		Status:      model.StatusWaitPay,
		NotifyURL:   "http://example.com/notify",
		CreatedAt:   now.Add(-40 * time.Minute), // 已超 30min
	}
	if err := st.CreateOrder(ctx, o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	svc.SetCloseWindow(30)

	// 商家通道无可用查单通道 → queryOrderFor 返回 ErrNotQueryable → 视为未支付关单
	closed, err := svc.CloseExpiredOrders(ctx, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("CloseExpiredOrders err: %v", err)
	}
	if closed != 1 {
		t.Fatalf("商家订单已到固定窗口应关单：closed=%d", closed)
	}
	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	if got.Status != model.StatusClosed {
		t.Fatalf("商家订单应已关闭：%s", got.Status)
	}
}

// stubMerchantChannel 商家通道桩（测试用）：ConfirmByQuery，QueryOrder 返回未支付。
type stubMerchantChannel struct{}

func (stubMerchantChannel) Name() string              { return "alipay" }
func (stubMerchantChannel) Type() channel.ChannelType { return channel.TypeMerchant }
func (stubMerchantChannel) Kind() channel.ChannelKind { return channel.KindCallback }
func (stubMerchantChannel) Modes() []channel.PayMode {
	return []channel.PayMode{channel.ModePage, channel.ModeQRCode}
}
func (stubMerchantChannel) ConfirmAbility() channel.ConfirmAbility { return channel.ConfirmByQuery }
func (stubMerchantChannel) Provider() channel.Provider             { return channel.ProviderAlipay }
func (stubMerchantChannel) CreatePayment(context.Context, *channel.PayRequest) (*channel.PayCredential, error) {
	return nil, nil
}
func (stubMerchantChannel) VerifyNotify(context.Context, *http.Request) (*channel.NotifyResult, error) {
	return nil, nil
}
func (stubMerchantChannel) QueryOrder(context.Context, string) (*channel.OrderStatus, error) {
	// 模拟官方通道查单：订单仍处于"未支付"状态
	return &channel.OrderStatus{Status: model.StatusWaitPay}, nil
}
func (stubMerchantChannel) Listener() channel.EventListener { return nil }

// registerStubMerchant 注册商家通道桩（幂等：已注册跳过）
func registerStubMerchant(t *testing.T) {
	t.Helper()
	if _, ok := channel.Lookup("alipay"); ok {
		return
	}
	channel.Register(stubMerchantChannel{})
}

// TestCloseExpiredOrders_PersonalOnline_StalledListener 监听端在线但此前已进过 LISTEN_TIMEOUT 异常池
// （复查过仍无推送，监听端僵死风险）→ 扫描器不直接关单，交由人工核实（P2-1 修复）。
func TestCloseExpiredOrders_PersonalOnline_StalledListener(t *testing.T) {
	registerStubPersonal(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	now := time.Now()
	o := &model.Order{
		OutTradeNo:  "P_STALLED",
		GatewayNo:   "GW_P_STALLED",
		Channel:     "personal.alipay",
		Pid:         "2001",
		AmountMinor: 1000,
		Currency:    "CNY",
		Subject:     "测试",
		Status:      model.StatusWaitPay,
		NotifyURL:   "http://example.com/notify",
		CreatedAt:   now.Add(-40 * time.Minute),
		DeviceID:    "dev-online",
	}
	if err := st.CreateOrder(ctx, o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	// 设备在线（返回 offline=false）
	svc.SetPersonalCheck(func(ctx context.Context, o *model.Order) (bool, string) {
		return false, ""
	})

	// 先模拟 reconcile 复查路径已将该单进过 LISTEN_TIMEOUT 异常池
	exc := &model.ExceptionOrder{
		OutTradeNo:  o.OutTradeNo,
		Pid:         o.Pid,
		Channel:     o.Channel,
		AmountMinor: o.AmountMinor,
		OrderStatus: o.Status,
		Reason:      model.ExReasonListenTimeout,
		Suggestion:  "监听端在线但长时间未收到收款推送，需人工确认实际收款情况",
	}
	if err := st.CreateException(ctx, exc); err != nil {
		t.Fatalf("预置异常记录失败: %v", err)
	}

	closed, err := svc.CloseExpiredOrders(ctx, now.Add(-30*time.Minute))
	if err != nil {
		t.Fatalf("CloseExpiredOrders err: %v", err)
	}
	if closed != 0 {
		t.Fatalf("监听端僵死风险订单不应关单：closed=%d", closed)
	}
	got, _ := st.GetOrder(ctx, o.OutTradeNo)
	if got.Status != model.StatusClosed {
		t.Logf("订单保持 WAIT_PAY（转人工核实路径）")
	} else {
		t.Fatalf("监听端僵死风险订单被误关（漏单！）：%s", got.Status)
	}
}

// stubQueryFailChannel 商家通道桩：关单前查单必失败（模拟沙箱/凭据异常）。
type stubQueryFailChannel struct{ name string }

func (c stubQueryFailChannel) Name() string                           { return c.name }
func (c stubQueryFailChannel) Type() channel.ChannelType              { return channel.TypeMerchant }
func (c stubQueryFailChannel) Kind() channel.ChannelKind              { return channel.KindCallback }
func (c stubQueryFailChannel) Modes() []channel.PayMode               { return []channel.PayMode{channel.ModeQRCode} }
func (c stubQueryFailChannel) ConfirmAbility() channel.ConfirmAbility { return channel.ConfirmByQuery }
func (c stubQueryFailChannel) Provider() channel.Provider             { return channel.ProviderAlipay }
func (c stubQueryFailChannel) CreatePayment(context.Context, *channel.PayRequest) (*channel.PayCredential, error) {
	return nil, nil
}
func (c stubQueryFailChannel) VerifyNotify(context.Context, *http.Request) (*channel.NotifyResult, error) {
	return nil, nil
}
func (c stubQueryFailChannel) QueryOrder(context.Context, string) (*channel.OrderStatus, error) {
	return nil, fmt.Errorf("所有支付宝账户查询均失败")
}
func (c stubQueryFailChannel) Listener() channel.EventListener { return nil }

// TestCloseExpiredOrders_QueryFail_EntersPoolOnce 查单失败风暴抑制：
// 首次扫描失败 → 入人工池（close_uncertain）一次；后续扫描静默跳过，不再重复入池。
func TestCloseExpiredOrders_QueryFail_EntersPoolOnce(t *testing.T) {
	chName := "merchant.queryfail"
	if _, ok := channel.Lookup(chName); !ok {
		channel.Register(stubQueryFailChannel{name: chName})
	}
	svc, st, _, _ := newTestService(t)
	svc.SetCloseWindow(30)
	ctx := context.Background()
	now := time.Now()

	o := &model.Order{
		OutTradeNo: "M_QFAIL", GatewayNo: "GW_M_QFAIL", Channel: chName,
		Pid: "2001", AmountMinor: 1000, Currency: "CNY", Subject: "测试",
		Status: model.StatusWaitPay, CreatedAt: now.Add(-40 * time.Minute),
	}
	if err := st.CreateOrder(ctx, o); err != nil {
		t.Fatalf("下单失败: %v", err)
	}

	// 第一次扫描：查单失败 → 入池一次，订单不关
	closed, err := svc.CloseExpiredOrders(ctx, now)
	if err != nil {
		t.Fatalf("扫描 err: %v", err)
	}
	if closed != 0 {
		t.Fatalf("查单失败不应关单, closed=%d", closed)
	}
	got, _ := st.GetOrder(ctx, "M_QFAIL")
	if got.Status != model.StatusWaitPay {
		t.Fatalf("查单失败订单保持 WAIT_PAY：%s", got.Status)
	}
	exc, err := st.GetExceptionByOutTradeNo(ctx, "M_QFAIL")
	if err != nil || exc == nil {
		t.Fatalf("首次查单失败应入人工池: err=%v exc=%v", err, exc)
	}
	if exc.Reason != model.ExReasonCloseUncertain {
		t.Fatalf("异常原因应 close_uncertain，实际 %s", exc.Reason)
	}

	// 第二次扫描：已有记录 → 静默跳过，不再重复入池（风暴抑制断言）
	before, err := st.CountExceptions(ctx, "")
	if err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	_, err = svc.CloseExpiredOrders(ctx, now)
	if err != nil {
		t.Fatalf("第二次扫描 err: %v", err)
	}
	after, err := st.CountExceptions(ctx, "")
	if err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if after != before {
		t.Fatalf("第二次扫描不应再入池：before=%d after=%d", before, after)
	}
}

// stubPaymentChannel 返回有效凭据的通道桩，用于测试复用更新金额触发凭据刷新（RefreshPayment/SwitchChannel/ReviveOrder）。
type stubPaymentChannel struct{}

func (stubPaymentChannel) Name() string                           { return "personal.test" }
func (stubPaymentChannel) Type() channel.ChannelType              { return channel.TypePersonal }
func (stubPaymentChannel) Kind() channel.ChannelKind              { return channel.KindListener }
func (stubPaymentChannel) Modes() []channel.PayMode               { return []channel.PayMode{channel.ModeQRCode} }
func (stubPaymentChannel) ConfirmAbility() channel.ConfirmAbility { return channel.ConfirmByManual }
func (stubPaymentChannel) Provider() channel.Provider             { return channel.ProviderAlipay }
func (stubPaymentChannel) CreatePayment(context.Context, *channel.PayRequest) (*channel.PayCredential, error) {
	return &channel.PayCredential{QRCode: "psn://test", AccountID: 1}, nil
}
func (stubPaymentChannel) VerifyNotify(context.Context, *http.Request) (*channel.NotifyResult, error) {
	return nil, nil
}
func (stubPaymentChannel) QueryOrder(context.Context, string) (*channel.OrderStatus, error) {
	return nil, ErrNotQueryable
}
func (stubPaymentChannel) Listener() channel.EventListener { return nil }

// registerStubPayment 注册返回有效凭据的通道桩（幂等：已注册跳过）
func registerStubPayment(t *testing.T) {
	t.Helper()
	if _, ok := channel.Lookup("personal.test"); ok {
		return
	}
	channel.Register(stubPaymentChannel{})
}

// createWaitPayOrderWithCred 创建带凭据的 WAIT_PAY 订单，并通过 GetOrder 返回带 ID 的最新对象（模拟真实调用链）。
func createWaitPayOrderWithCred(t *testing.T, st *store.SQLiteStore, outTradeNo, ch string, amount int64) *model.Order {
	t.Helper()
	o := &model.Order{
		OutTradeNo:  outTradeNo,
		GatewayNo:   "GW" + outTradeNo,
		Channel:     ch,
		Pid:         "2001",
		AmountMinor: amount,
		Currency:    "CNY",
		Subject:     "测试",
		Status:      model.StatusWaitPay,
		NotifyURL:   "http://example.com/notify",
		QrCode:      "old://qr",
		CreatedAt:   time.Now(),
	}
	if err := st.CreateOrder(context.Background(), o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	latest, err := st.GetOrder(context.Background(), outTradeNo)
	if err != nil {
		t.Fatalf("读回订单失败: %v", err)
	}
	return latest
}

// TestReuseOrder_AmountChanged_Refresh 同订单号复用传不同金额 → 金额不可变，拒单。
// 回归：旧行为覆盖已存在订单金额（UpdateOrderAmount）+ 以新金额刷新凭据；该行为会导致
// 回调按订单金额严校验时实付/订单不一致而永不入账，且订单号+金额是上游不可变主键，
// 同号改金额应由上游换新单号（买家级替换，见 CreateOrder 守卫）而非覆盖复用。
func TestReuseOrder_AmountChanged_Refresh(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	o := createWaitPayOrderWithCred(t, st, "REUSE_AMT", "personal.test", 100)

	_, err := svc.reuseOrCreateError(context.Background(), o, "personal.test", 200)
	if err == nil {
		t.Fatal("同号不同金额应拒单（金额不可变）")
	}

	got, _ := st.GetOrder(context.Background(), o.OutTradeNo)
	if got.AmountMinor != 100 {
		t.Fatalf("金额不可变：DB 订单金额 = %d, 期望保持 100（未被覆盖）", got.AmountMinor)
	}
	if got.QrCode != "old://qr" {
		t.Fatalf("金额不可变：不应刷新凭据，%q", got.QrCode)
	}
	if got.Status != model.StatusWaitPay {
		t.Fatalf("金额不可变：订单不应被改状态，status=%s", got.Status)
	}
	evs, _ := st.ListEvents(context.Background(), got.ID, 10)
	for _, e := range evs {
		if e.EventType == "amount_updated" {
			t.Fatal("不应再记录 amount_updated 事件（金额不可变）")
		}
	}
}

// TestReuseOrder_AmountSame_NoRefresh 复用订单传入相同金额 → 纯复用，不刷新凭据、不改金额。
func TestReuseOrder_AmountSame_NoRefresh(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	o := createWaitPayOrderWithCred(t, st, "REUSE_SAME", "personal.test", 100)

	res, err := svc.reuseOrCreateError(context.Background(), o, "personal.test", 100)
	if err != nil {
		t.Fatalf("reuseOrCreateError err: %v", err)
	}
	if !res.Reused {
		t.Fatal("应标记为复用")
	}
	if res.Order.AmountMinor != 100 {
		t.Fatalf("同金额复用订单金额 = %d, 期望 100", res.Order.AmountMinor)
	}
	got, _ := st.GetOrder(context.Background(), o.OutTradeNo)
	if got.QrCode != "old://qr" {
		t.Fatalf("同金额复用不应刷新凭据：%q", got.QrCode)
	}
}

// TestReuseOrder_AmountChanged_ClosedRevive 已关单订单同号复用传新金额 → 金额不可变，拒单（复活前置金额校验）。
func TestReuseOrder_AmountChanged_ClosedRevive(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	_ = createWaitPayOrderWithCred(t, st, "REUSE_CLOSED", "personal.test", 100)
	if ok, err := svc.CloseOrder(context.Background(), "REUSE_CLOSED", "timeout"); err != nil || !ok {
		t.Fatalf("关单失败: ok=%v err=%v", ok, err)
	}
	closed, _ := st.GetOrder(context.Background(), "REUSE_CLOSED")

	_, err := svc.reuseOrCreateError(context.Background(), closed, "personal.test", 300)
	if err == nil {
		t.Fatal("同号不同金额应拒单（即使已关单，金额仍不可变）")
	}
	got, _ := st.GetOrder(context.Background(), "REUSE_CLOSED")
	if got.AmountMinor != 100 {
		t.Fatalf("金额不可变：DB 金额 = %d, 期望保持 100", got.AmountMinor)
	}
	if got.Status != model.StatusClosed {
		t.Fatalf("金额不可变：拒单后订单不应被复活，status=%s", got.Status)
	}
}

// TestReuseOrder_AmountChanged_SwitchChannel 复用传新金额且换支付方式 → 金额不可变，拒单（通道切换前金额校验）。
func TestReuseOrder_AmountChanged_SwitchChannel(t *testing.T) {
	registerStubPersonal(t) // 原通道 personal.alipay（返回 nil 凭据，仅作 from 侧）
	registerStubPayment(t)  // 目标通道 personal.test（返回有效凭据）
	svc, st, _, _ := newTestService(t)
	o := createWaitPayOrderWithCred(t, st, "REUSE_SWITCH", "personal.alipay", 100)

	_, err := svc.reuseOrCreateError(context.Background(), o, "personal.test", 400)
	if err == nil {
		t.Fatal("同号不同金额应拒单（即使要切换通道，金额仍不可变）")
	}
	got, _ := st.GetOrder(context.Background(), o.OutTradeNo)
	if got.Channel != "personal.alipay" {
		t.Fatalf("金额不可变：拒单后通道不应被切换，channel=%s", got.Channel)
	}
	if got.AmountMinor != 100 {
		t.Fatalf("金额不可变：DB 金额 = %d, 期望保持 100", got.AmountMinor)
	}
}

// TestCreateOrder_BuyerPendingNewTrade_ReplacesOld 买家级串行：同一 buyer_key 已有 WAIT_BUYER_PAY
// 待支付订单(OLD)，又以新 out_trade_no 提交新订单(NEW) 时，应废弃旧单、返回新单。
// Given: OLD 为 WAIT_BUYER_PAY @10.00 元、buyer_key=buyer-replace、通道 personal.test 已带凭据；
//
//	NEW 为同 buyer_key、不同 out_trade_no、金额 1.00 元。
//
// When:  Service.CreateOrder(NEW)
// Then:  返回订单用 NEW(out_trade_no=NEW)、金额 100 分；OLD 已被关闭；NEW 存在于 DB。
// 回归：当前 CreateOrder 买家守卫(service.go:205-227)错误地复用 OLD 并覆盖其金额，返回旧单。
func TestCreateOrder_BuyerPendingNewTrade_ReplacesOld(t *testing.T) {
	registerStubPayment(t) // personal.test 返回有效凭据
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	// Given: OLD_ORDER WAIT_BUYER_PAY @10.00元，同 buyer_key，已带凭据
	old := createWaitPayOrderWithCred(t, st, "OLD_REPLACE", "personal.test", 1000)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		upd.BuyerKey = "buyer-replace"
	}); err != nil || !ok {
		t.Fatalf("设置 OLD buyer_key 失败: ok=%v err=%v", ok, err)
	}
	old, _ = st.GetOrder(ctx, old.OutTradeNo)
	if old.BuyerKey != "buyer-replace" {
		t.Fatalf("前置条件：OLD buyer_key 未生效: %q", old.BuyerKey)
	}

	// When: 以新单号提交 NEW_ORDER @1.00元
	res, err := svc.CreateOrder(ctx, &CreateRequest{
		OutTradeNo: "NEW_REPLACE",
		Type:       "personal.test",
		Name:       "新单",
		Money:      "1.00",
		NotifyURL:  "http://example.com/notify",
		BuyerKey:   "buyer-replace",
	})
	if err != nil {
		t.Fatalf("CreateOrder err: %v", err)
	}

	// Then: 返回订单为新单，金额 100 分
	if res == nil || res.Order == nil {
		t.Fatal("CreateOrder 返回空结果")
	}
	if res.Order.OutTradeNo != "NEW_REPLACE" {
		t.Fatalf("返回订单 = %s, 期望新单 NEW_REPLACE（当前错误复用了旧单）", res.Order.OutTradeNo)
	}
	if res.Order.AmountMinor != 100 {
		t.Fatalf("返回订单金额 = %d, 期望 100", res.Order.AmountMinor)
	}

	// Then: OLD 已被关闭
	gotOld, _ := st.GetOrder(ctx, "OLD_REPLACE")
	if gotOld == nil {
		t.Fatal("OLD_REPLACE 应存在")
	}
	if gotOld.Status != model.StatusClosed {
		t.Fatalf("OLD 状态 = %s, 期望 CLOSED（旧单应被关闭，而非继续复用）", gotOld.Status)
	}

	// Then: NEW 存在于 DB
	gotNew, err := st.GetOrder(ctx, "NEW_REPLACE")
	if err != nil || gotNew == nil {
		t.Fatalf("NEW_REPLACE 应存在于 DB, err=%v", err)
	}
}

// TestCreateOrder_BuyerReplacement_InheritsSessionExpiry 换金额不能重置支付会话总截止时间。
func TestCreateOrder_BuyerReplacement_InheritsSessionExpiry(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	svc.SetCloseWindow(30)
	expires := time.Now().Add(7 * time.Minute).Round(0)
	old := createWaitPayOrderWithCred(t, st, "OLD_SESSION", "personal.test", 1000)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		upd.BuyerKey = "buyer-session"
		upd.SessionExpiresAt = expires
	}); err != nil || !ok {
		t.Fatalf("设置会话截止时间失败: ok=%v err=%v", ok, err)
	}

	res, err := svc.CreateOrder(ctx, &CreateRequest{
		OutTradeNo: "NEW_SESSION",
		Type:       "personal.test",
		Name:       "新单",
		Money:      "1.00",
		NotifyURL:  "http://example.com/notify",
		BuyerKey:   "buyer-session",
	})
	if err != nil {
		t.Fatalf("替换下单失败: %v", err)
	}
	got, _ := st.GetOrder(ctx, res.Order.OutTradeNo)
	if !got.SessionExpiresAt.Equal(expires) {
		t.Fatalf("新订单会话截止时间 = %v, 期望继承 %v", got.SessionExpiresAt, expires)
	}
	if !got.CreatedAt.Before(got.SessionExpiresAt) {
		t.Fatalf("新订单创建时间必须早于会话截止时间: created=%v expires=%v", got.CreatedAt, got.SessionExpiresAt)
	}
}

// TestCreateOrder_BuyerReplacement_ExpiredSessionRejects 换金额不能复活已过期支付会话。
func TestCreateOrder_BuyerReplacement_ExpiredSessionRejects(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	old := createWaitPayOrderWithCred(t, st, "OLD_EXPIRED_SESSION", "personal.test", 1000)
	expires := time.Now().Add(-time.Minute)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		upd.BuyerKey = "buyer-expired-session"
		upd.SessionExpiresAt = expires
	}); err != nil || !ok {
		t.Fatalf("设置过期会话失败: ok=%v err=%v", ok, err)
	}

	_, err := svc.CreateOrder(ctx, &CreateRequest{
		OutTradeNo: "NEW_EXPIRED_SESSION",
		Type:       "personal.test",
		Name:       "新单",
		Money:      "1.00",
		NotifyURL:  "http://example.com/notify",
		BuyerKey:   "buyer-expired-session",
	})
	if err == nil {
		t.Fatal("过期支付会话不应继续替换创建新 WAIT_PAY")
	}
	if got, _ := st.GetOrder(ctx, "NEW_EXPIRED_SESSION"); got != nil {
		t.Fatal("过期会话拒绝后不应创建新订单")
	}
}

// TestBuildNotifyPayload_UsesLatestNotifyURL 死信重发核心：BuildNotifyPayload 反映订单最新 NotifyURL。
// 回归：死信重发此前固化任务创建时的旧地址 → 一直回调不了；此测试锁住「改对地址后重建 payload 用最新值」。
func TestBuildNotifyPayload_UsesLatestNotifyURL(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	o := &model.Order{
		GatewayNo:   "GW123",
		OutTradeNo:  "OTN123",
		Channel:     "alipay",
		Subject:     "测试",
		AmountMinor: 100,
		NotifyURL:   "http://old.example/cb",
	}
	var p map[string]string
	parse := func(payload string) map[string]string {
		m := map[string]string{}
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("payload 解析失败: %v", err)
		}
		return m
	}

	p = parse(svc.BuildNotifyPayload(o))
	if p["notify_url"] != "http://old.example/cb" {
		t.Fatalf("notify_url = %q, 期望旧地址", p["notify_url"])
	}
	if p["sign"] == "" {
		t.Fatal("payload 应含 sign")
	}
	if p["money"] != "1.00" {
		t.Fatalf("money = %q, 期望 1.00", p["money"])
	}

	// 订单回调地址改对后，重建 payload 应反映最新地址
	o.NotifyURL = "http://new.example/cb"
	p = parse(svc.BuildNotifyPayload(o))
	if p["notify_url"] != "http://new.example/cb" {
		t.Fatalf("notify_url = %q, 期望最新地址 http://new.example/cb", p["notify_url"])
	}
}

// TestCreateOrder_SameOrderNumber_DifferentAmount_Rejected 同订单号但金额不同 → 金额不可变，拒单。
// Given: 已存在订单 SAME_NO @10.00元。
// When:  以同订单号 + 1.00 元重提。
// Then:  返回错误；DB 金额保持 1000；订单仍 WAIT_PAY；无 amount_updated 事件、无凭据刷新。
func TestCreateOrder_SameOrderNumber_DifferentAmount_Rejected(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	createWaitPayOrderWithCred(t, st, "SAME_AMT", "personal.test", 1000)

	_, err := svc.CreateOrder(ctx, &CreateRequest{
		OutTradeNo: "SAME_AMT",
		Type:       "personal.test",
		Name:       "重提",
		Money:      "1.00",
		NotifyURL:  "http://example.com/notify",
		BuyerKey:   "buyer-same",
	})
	if err == nil {
		t.Fatal("同号不同金额应拒单（金额不可变）")
	}

	got, _ := st.GetOrder(ctx, "SAME_AMT")
	if got.AmountMinor != 1000 {
		t.Fatalf("金额不可变：DB 金额 = %d, 期望保持 1000", got.AmountMinor)
	}
	if got.QrCode != "old://qr" {
		t.Fatalf("金额不可变：不应刷新凭据，%q", got.QrCode)
	}
	if got.Status != model.StatusWaitPay {
		t.Fatalf("金额不可变：订单应保持 WAIT_PAY，status=%s", got.Status)
	}
	evs, _ := st.ListEvents(ctx, got.ID, 10)
	for _, e := range evs {
		if e.EventType == "amount_updated" || e.EventType == "replaced" {
			t.Fatalf("同号不同金额不应产生 %s 事件", e.EventType)
		}
	}
}

// TestCreateOrder_SameOrderNumber_SameAmount_Reuse 同订单号且金额相同 → 幂等复用，不新建。
// Given: 已存在订单 SAME_OK @10.00元（带凭据）。
// When:  以同订单号 + 同金额 10.00 元重提。
// Then:  返回 Reused=true，返回原订单（out_trade_no 相同、金额不变、凭据不变）。
func TestCreateOrder_SameOrderNumber_SameAmount_Reuse(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	old := createWaitPayOrderWithCred(t, st, "SAME_OK", "personal.test", 1000)

	res, err := svc.CreateOrder(ctx, &CreateRequest{
		OutTradeNo: "SAME_OK",
		Type:       "personal.test",
		Name:       "重提",
		Money:      "10.00",
		NotifyURL:  "http://example.com/notify",
		BuyerKey:   "buyer-same-ok",
	})
	if err != nil {
		t.Fatalf("CreateOrder err: %v", err)
	}
	if res == nil || !res.Reused {
		t.Fatalf("同号同金额应复用 Reused=true, res=%+v", res)
	}
	if res.Order.OutTradeNo != "SAME_OK" || res.Order.AmountMinor != 1000 {
		t.Fatalf("复用应返回原单 SAME_OK@1000，got %s@%d", res.Order.OutTradeNo, res.Order.AmountMinor)
	}
	got, _ := st.GetOrder(ctx, "SAME_OK")
	if got.AmountMinor != 1000 || got.QrCode != "old://qr" {
		t.Fatalf("同号同金额复用不应改金额/凭据：amount=%d qr=%q", got.AmountMinor, got.QrCode)
	}
	if got.ID != old.ID {
		t.Fatalf("同号同金额应复用同一订单记录，id=%d vs old=%d", got.ID, old.ID)
	}
}

// TestCreateOrder_BuyerReplacement_EventAndCleanup 买家替换旧单 → replaced 事件恰一条、备注码释放一次、池移除一次。
// Given: OLD_REPL2 WAIT_PAY @10.00元、buyer_key、带 NoteCode。
// When:  CreateOrder(NEW_REPL2 @1.00元)。
// Then:  OLD 被关闭；写且仅写一条 replaced 事件；备注码释放一次；池移除一次；NEW 以新金额/新单号持久化。
func TestCreateOrder_BuyerReplacement_EventAndCleanup(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	released := 0
	removed := 0
	svc.SetNoteCodeReleaser(func(ch, code string) { released++ })
	svc.SetWaitPayHooks(&WaitPayHooks{
		Remove: func(outTradeNo string) { removed++ },
	})

	old := createWaitPayOrderWithCred(t, st, "OLD_REPL2", "personal.test", 1000)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		upd.BuyerKey = "buyer-repl2"
		upd.NoteCode = "NC-REPL2"
		upd.AccountID = 1
	}); err != nil || !ok {
		t.Fatalf("设置 OLD buyer_key/note_code 失败: %v %v", ok, err)
	}

	res, err := svc.CreateOrder(ctx, &CreateRequest{
		OutTradeNo: "NEW_REPL2",
		Type:       "personal.test",
		Name:       "新单",
		Money:      "1.00",
		NotifyURL:  "http://example.com/notify",
		BuyerKey:   "buyer-repl2",
	})
	if err != nil {
		t.Fatalf("CreateOrder err: %v", err)
	}
	if res.Order.OutTradeNo != "NEW_REPL2" || res.Order.AmountMinor != 100 {
		t.Fatalf("返回应为新单 NEW_REPL2@100，got %s@%d", res.Order.OutTradeNo, res.Order.AmountMinor)
	}

	gotOld, _ := st.GetOrder(ctx, "OLD_REPL2")
	if gotOld.Status != model.StatusClosed {
		t.Fatalf("OLD 应 CLOSED，got %s", gotOld.Status)
	}
	evs, _ := st.ListEvents(ctx, gotOld.ID, 10)
	replaced := 0
	for _, e := range evs {
		if e.EventType == "replaced" {
			replaced++
			if !json.Valid([]byte(e.RawPayload)) {
				t.Fatalf("replaced 事件 payload 非法 JSON: %s", e.RawPayload)
			}
		}
	}
	if replaced != 1 {
		t.Fatalf("replaced 事件条数 = %d, 期望恰 1", replaced)
	}
	if released != 1 {
		t.Fatalf("备注码应释放恰 1 次，got %d", released)
	}
	if removed != 1 {
		t.Fatalf("WAIT_PAY 池应移除恰 1 次，got %d", removed)
	}
	gotNew, _ := st.GetOrder(ctx, "NEW_REPL2")
	if gotNew == nil || gotNew.AmountMinor != 100 {
		t.Fatalf("NEW 应以新金额持久化：%+v", gotNew)
	}
}

// TestCreateOrder_BuyerReplacement_CallbackIdentity 替换后新单回调 payload 指向新单号/新金额。
// When:  买家替换后创建 NEW。
// Then:  NEW 的 BuildNotifyPayload 的 out_trade_no/money 反映 NEW，而非 OLD。
func TestCreateOrder_BuyerReplacement_CallbackIdentity(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	old := createWaitPayOrderWithCred(t, st, "OLD_CB", "personal.test", 1000)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		upd.BuyerKey = "buyer-cb"
	}); err != nil || !ok {
		t.Fatalf("设置 OLD buyer_key 失败: %v %v", ok, err)
	}

	res, err := svc.CreateOrder(ctx, &CreateRequest{
		OutTradeNo: "NEW_CB",
		Type:       "personal.test",
		Name:       "新单",
		Money:      "0.05",
		NotifyURL:  "http://example.com/notify",
		BuyerKey:   "buyer-cb",
	})
	if err != nil {
		t.Fatalf("CreateOrder err: %v", err)
	}

	var params map[string]string
	if err := json.Unmarshal([]byte(svc.BuildNotifyPayload(res.Order)), &params); err != nil {
		t.Fatalf("payload 解析失败: %v", err)
	}
	if params["out_trade_no"] != "NEW_CB" {
		t.Fatalf("回调 out_trade_no = %q, 期望 NEW_CB（不能引用 OLD）", params["out_trade_no"])
	}
	if params["money"] != "0.05" {
		t.Fatalf("回调 money = %q, 期望 0.05（新单金额）", params["money"])
	}
}

// TestReplacePendingOrder_AlreadyClosed_NoEvent OLD 已被并发替换关闭 → 替换幂等：不重复写 replaced 事件，仍可继续创建 NEW。
func TestReplacePendingOrder_AlreadyClosed_NoEvent(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	old := createWaitPayOrderWithCred(t, st, "OLD_CLOSED", "personal.test", 1000)

	if ok, err := svc.CloseOrder(ctx, old.OutTradeNo, "timeout"); err != nil || !ok {
		t.Fatalf("预置 OLD 为 CLOSED 失败: %v %v", ok, err)
	}

	_, create, err := svc.replacePendingOrder(ctx, old, "NEW_CLOSED", 100)
	if err != nil {
		t.Fatalf("replacePendingOrder err: %v", err)
	}
	if !create {
		t.Fatal("OLD 已被关闭（非结算）时仍应继续创建 NEW")
	}
	evs, _ := st.ListEvents(ctx, old.ID, 10)
	for _, e := range evs {
		if e.EventType == "replaced" {
			t.Fatal("OLD 已被并发替换关闭时不应再写 replaced 事件（幂等）")
		}
	}
}

// TestReplacePendingOrder_ConcurrentSettlement_NoProceed OLD 已被并发结算(SUCCESS) → 替换 CAS 失败，返回 proceed=false，禁止创建新单。
func TestReplacePendingOrder_ConcurrentSettlement_NoProceed(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	old := createWaitPayOrderWithCred(t, st, "OLD_SETTLED", "personal.test", 1000)

	if ok, err := svc.SettlePayment(ctx, &channel.NotifyResult{OutTradeNo: old.OutTradeNo, ChannelTxID: "TX-SETTLED", AmountMinor: 1000}); err != nil || !ok {
		t.Fatalf("预置 OLD 结算失败: %v %v", ok, err)
	}

	_, create, err := svc.replacePendingOrder(ctx, old, "NEW_SETTLED", 100)
	if err != nil {
		t.Fatalf("replacePendingOrder err: %v", err)
	}
	if create {
		t.Fatal("OLD 已被并发结算胜出时不得创建新单（防重复收款）")
	}
	// CAS 失败 → 订单保持 SUCCESS，不写 replaced，不移除池/释放码
	got, _ := st.GetOrder(ctx, old.OutTradeNo)
	if got.Status != model.StatusSuccess {
		t.Fatalf("OLD 应保持 SUCCESS，got %s", got.Status)
	}
	evs, _ := st.ListEvents(ctx, old.ID, 10)
	for _, e := range evs {
		if e.EventType == "replaced" {
			t.Fatal("结算胜出后不应写 replaced 事件")
		}
	}
}

// TestCreateOrder_BuyerReplacement_AfterSettleAllowsNew OLD 已被结算(SUCCESS)后，买家可正常下新单。
// Given: OLD 已结算入账（不再是 WAIT_PAY，不占用买家串行锁）。
// When:  CreateOrder 以同 buyer_key + 新单号提交。
// Then:  成功创建 NEW（已支付旧单不阻塞新单；替换仅在 OLD 仍 WAIT_PAY 的竞态窗口内通过 CAS 拒绝，见单测）。
func TestCreateOrder_BuyerReplacement_AfterSettleAllowsNew(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	old := createWaitPayOrderWithCred(t, st, "OLD_SETTLED3", "personal.test", 1000)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(upd *model.Order) {
		upd.BuyerKey = "buyer-settled3"
	}); err != nil || !ok {
		t.Fatalf("设置 buyer_key 失败: %v %v", ok, err)
	}
	if ok, err := svc.SettlePayment(ctx, &channel.NotifyResult{OutTradeNo: old.OutTradeNo, ChannelTxID: "TX-S3", AmountMinor: 1000}); err != nil || !ok {
		t.Fatalf("预置结算失败: %v %v", ok, err)
	}

	res, err := svc.CreateOrder(ctx, &CreateRequest{
		OutTradeNo: "NEW_SETTLED3",
		Type:       "personal.test",
		Name:       "新单",
		Money:      "1.00",
		NotifyURL:  "http://example.com/notify",
		BuyerKey:   "buyer-settled3",
	})
	if err != nil {
		t.Fatalf("CreateOrder err（已支付旧单不阻塞新单）: %v", err)
	}
	if res.Order.OutTradeNo != "NEW_SETTLED3" {
		t.Fatalf("应创建新单 NEW_SETTLED3，got %s", res.Order.OutTradeNo)
	}
	got, _ := st.GetOrder(ctx, "NEW_SETTLED3")
	if got == nil {
		t.Fatal("NEW_SETTLED3 应存在于 DB")
	}
}

// TestReplacePendingOrder_Concurrent_Goroutine 真实 SQLite 并发：替代替换 与 结算 并行，
// 断言不变量：绝不双写 replaced、绝不双入账；OLD 终态必为 SUCCESS 或 CLOSED 之一。
func TestReplacePendingOrder_Concurrent_Goroutine(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	old := createWaitPayOrderWithCred(t, st, "OLD_RACE", "personal.test", 1000)

	start := make(chan struct{})
	done := make(chan struct{}, 2)

	go func() {
		<-start
		svc.SettlePayment(ctx, &channel.NotifyResult{OutTradeNo: old.OutTradeNo, ChannelTxID: "TX-RACE", AmountMinor: 1000})
		done <- struct{}{}
	}()
	go func() {
		<-start
		svc.replacePendingOrder(ctx, old, "NEW_RACE", 100)
		done <- struct{}{}
	}()
	close(start)
	<-done
	<-done

	got, _ := st.GetOrder(ctx, old.OutTradeNo)
	if got.Status != model.StatusSuccess && got.Status != model.StatusClosed {
		t.Fatalf("OLD 终态应为 SUCCESS 或 CLOSED，got %s", got.Status)
	}
	evs, _ := st.ListEvents(ctx, old.ID, 50)
	replaced := 0
	paid := 0
	for _, e := range evs {
		switch e.EventType {
		case "replaced":
			replaced++
		case "paid":
			paid++
		}
	}
	if replaced > 1 {
		t.Fatalf("replaced 事件最多写 1 条，got %d", replaced)
	}
	if paid > 1 {
		t.Fatalf("paid 事件最多写 1 条（不重复入账），got %d", paid)
	}
	if got.Status == model.StatusSuccess && replaced != 0 {
		t.Fatalf("结算胜出时不应写 replaced，got %d", replaced)
	}
}

// replaceLoserCreates 模拟并发替换者已原子关单 OLD（写 replaced 事件），然后把当前调用当作 CAS 落败者。
// OLD 代码下该场景 CAS 落败后会继续放行创建 NEW（proceed=true），酿成同买家多笔 WAIT_PAY；
// 修复后落败者必须 create=false（复用获胜 NEW 或不创建）。
func TestReplacePendingOrder_LoserNeverCreates(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	old := createWaitPayOrderWithCred(t, st, "OLD_LOSER", "personal.test", 1000)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(u *model.Order) {
		u.BuyerKey = "buyer-loser"
	}); err != nil || !ok {
		t.Fatalf("设置 buyer_key 失败: %v %v", ok, err)
	}
	old, _ = st.GetOrder(ctx, old.OutTradeNo)

	// When: 并发替换者已先原子关闭 OLD 并写入 replaced 事件（模拟 A 在 CAS 中胜出）。
	if ok, err := svc.store.ReplaceOrder(ctx, old.OutTradeNo, old.ID, old.AmountMinor, `{"new_out_trade_no":"WINNER_LOSER"}`); err != nil || !ok {
		t.Fatalf("模拟并发替换者关单失败: %v %v", ok, err)
	}

	// When: 当前调用作为 CAS 落败者尝试替换同一个 OLD。
	_, create, err := svc.replacePendingOrder(ctx, old, "NEW_LOSER", 200)
	if err == nil {
		t.Fatal("OLD 已被并发替换者关闭且获胜 NEW 未就绪时，落败者应返回可重试冲突错误")
	}
	if create {
		t.Fatal("CAS 落败者（OLD 已被并发替换者关闭）绝不能再创建自己的 NEW——违反单活跃 WAIT_PAY 不变量")
	}
}

// TestReplacePendingOrder_LoserReusesWinner OLD 已被并发替换者关闭且获胜 NEW 已可见时，落败者复用获胜 NEW。
func TestReplacePendingOrder_LoserReusesWinner(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	old := createWaitPayOrderWithCred(t, st, "OLD_REUSE", "personal.test", 1000)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(u *model.Order) {
		u.BuyerKey = "buyer-reuse"
	}); err != nil || !ok {
		t.Fatalf("设置 buyer_key 失败: %v %v", ok, err)
	}
	old, _ = st.GetOrder(ctx, old.OutTradeNo)

	// 并发替换者已关 OLD 并已创建获胜 NEW（同买家环境 WAIT_PAY）。
	if ok, err := svc.store.ReplaceOrder(ctx, old.OutTradeNo, old.ID, old.AmountMinor, `{"new_out_trade_no":"WINNER_REUSE"}`); err != nil || !ok {
		t.Fatalf("模拟并发替换者关单失败: %v %v", ok, err)
	}
	winner := createWaitPayOrderWithCred(t, st, "WINNER_REUSE", "personal.test", 300)
	if ok, err := st.UpdateOrderStatus(ctx, winner.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(u *model.Order) {
		u.BuyerKey = "buyer-reuse"
	}); err != nil || !ok {
		t.Fatalf("设置获胜 NEW buyer_key 失败: %v %v", ok, err)
	}

	reuse, create, err := svc.replacePendingOrder(ctx, old, "NEW_REUSE", 200)
	if err != nil {
		t.Fatalf("落败者应复用获胜 NEW 而非报错，got err: %v", err)
	}
	if create {
		t.Fatal("落败者不得创建自己的 NEW")
	}
	if reuse == nil || reuse.OutTradeNo != "WINNER_REUSE" {
		t.Fatalf("落败者应复用获胜 NEW WINNER_REUSE，got %+v", reuse)
	}
}

// TestReplacePendingOrder_ConcurrentLoser_Race 真实 SQLite 并发：两个请求同时替换同一 OLD。
// 断言：至多一方 create=true（绝不出现两方都放行创建 → 同买家双 WAIT_PAY）。
func TestReplacePendingOrder_ConcurrentLoser_Race(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	old := createWaitPayOrderWithCred(t, st, "OLD_RACE2", "personal.test", 1000)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(u *model.Order) {
		u.BuyerKey = "buyer-race2"
	}); err != nil || !ok {
		t.Fatalf("设置 buyer_key 失败: %v %v", ok, err)
	}
	old, _ = st.GetOrder(ctx, old.OutTradeNo)

	start := make(chan struct{})
	done := make(chan bool, 2)

	for i := 0; i < 2; i++ {
		go func() {
			<-start
			_, create, _ := svc.replacePendingOrder(ctx, old, "NEW_RACE2", int64(100*(i+1)))
			done <- create
		}()
	}
	close(start)
	creates := 0
	for i := 0; i < 2; i++ {
		if <-done {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("并发替换同一 OLD 时恰一方可创建 NEW，got %d（>1 产生同买家多笔 WAIT_PAY）", creates)
	}
}

// TestCreateOrder_BuyerSequentialReplacement_NoGhost 连续多次改价（不同订单号）：
// 每次替换关闭旧单、只保留一笔活跃 WAIT_PAY；历史订单为 CLOSED（允许存在），绝无活跃幽灵 WAIT_PAY。
func TestCreateOrder_BuyerSequentialReplacement_NoGhost(t *testing.T) {
	registerStubPayment(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()

	old := createWaitPayOrderWithCred(t, st, "OLD_SEQ", "personal.test", 1000)
	if ok, err := st.UpdateOrderStatus(ctx, old.OutTradeNo, model.StatusWaitPay, model.StatusWaitPay, func(u *model.Order) {
		u.BuyerKey = "buyer-seq"
	}); err != nil || !ok {
		t.Fatalf("设置 buyer_key 失败: %v %v", ok, err)
	}

	order := func(no, money string) {
		res, err := svc.CreateOrder(ctx, &CreateRequest{
			OutTradeNo: no, Type: "personal.test", Name: "新单", Money: money,
			NotifyURL: "http://example.com/notify", BuyerKey: "buyer-seq",
		})
		if err != nil {
			t.Fatalf("CreateOrder(%s) err: %v", no, err)
		}
		if res != nil && res.Order != nil && res.Order.OutTradeNo != no {
			t.Fatalf("顺序改价应生成新单 %s，got %s", no, res.Order.OutTradeNo)
		}
	}
	order("NEW_SEQ_1", "1.00")
	order("NEW_SEQ_2", "2.00")
	order("NEW_SEQ_3", "3.00")

	// Then: 恰一笔活跃 WAIT_PAY（最新 NEW_SEQ_3），其余均为 CLOSED。
	pend, err := st.ListWaitPayByBuyer(ctx, "buyer-seq")
	if err != nil {
		t.Fatalf("ListWaitPayByBuyer err: %v", err)
	}
	if len(pend) != 1 {
		t.Fatalf("连续改价后应仅剩 1 笔活跃 WAIT_PAY，got %d（存在活跃幽灵）", len(pend))
	}
	if pend[0].OutTradeNo != "NEW_SEQ_3" {
		t.Fatalf("活跃 WAIT_PAY 应为最新 NEW_SEQ_3，got %s", pend[0].OutTradeNo)
	}
	for _, no := range []string{"OLD_SEQ", "NEW_SEQ_1", "NEW_SEQ_2"} {
		got, _ := st.GetOrder(ctx, no)
		if got == nil || got.Status != model.StatusClosed {
			t.Fatalf("历史订单 %s 应已 CLOSED（非活跃幽灵），got %v", no, got)
		}
	}
}
