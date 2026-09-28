package main

// 资金核心链路测试：账本新收款行 → 订单匹配 → 入账（dispatchLedgerRows / matchPersonalOrder）。
// 覆盖资金安全关键分支：唯一匹配入账、多候选拒绝(ambiguous)、无订单(no_order)、
// 冻结账户守卫（不自动入账）、重复去重（幂等）、通道/账户归属校验。

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/admin/capture"
	"paygateway/internal/channel"
	"paygateway/internal/ledger"
	"paygateway/internal/model"
	"paygateway/internal/order"
	"paygateway/internal/personal"
	"paygateway/internal/pool"
	"paygateway/internal/reconciler"
	"paygateway/internal/store"
	"paygateway/internal/upstream"
)

// newMatchTestEnv 构建匹配链路测试环境：业务库 + personal.db + 账户池 + 订单服务 + App
func newMatchTestEnv(t *testing.T) (*App, *store.PersonalDB) {
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

// newTestDevice 创建设备（含加密密钥）+ 绑定账户到池
func newTestDevice(t *testing.T, pdb *store.PersonalDB, pool *account.AccountPool, accountID int64, deviceID string) *personal.Device {
	t.Helper()
	secretEnc, err := account.Encrypt([]byte("test-secret-1234567890"))
	if err != nil {
		t.Fatalf("加密设备密钥失败: %v", err)
	}
	rowID, err := pdb.CreateDevice(context.Background(), &personal.DeviceEncrypted{
		DeviceID: deviceID, Name: "测试机", AccountID: accountID,
		Provider: "alipay", SecretEnc: secretEnc, Status: string(personal.DeviceOnline),
	})
	if err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}
	return &personal.Device{ID: rowID, DeviceID: deviceID, Name: "测试机", AccountID: accountID, Provider: "alipay", Secret: "test-secret-1234567890", Status: personal.DeviceOnline}
}

// newTestOrder 创建一笔 WAIT_PAY 订单
func newTestOrder(t *testing.T, st store.Store, outNo string, amountMinor int64, accountID int64) {
	t.Helper()
	newTestOrderNote(t, st, outNo, amountMinor, accountID, "")
}

// newTestOrderNote 创建一笔 WAIT_PAY 订单（可带备注码）
func newTestOrderNote(t *testing.T, st store.Store, outNo string, amountMinor int64, accountID int64, noteCode string) {
	t.Helper()
	o := &model.Order{
		OutTradeNo: outNo, GatewayNo: "G-" + outNo, Channel: "personal.alipay",
		AccountID: accountID, AmountMinor: amountMinor, Status: model.StatusWaitPay,
		NoteCode: noteCode, CreatedAt: time.Now(),
	}
	if err := st.CreateOrder(context.Background(), o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
}

func TestDispatchLedgerRows_MatchAndSettle(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)

	// 账户池：两个支付宝账户（主 + 附加，双开场景）
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
		QRContent: "https://qr.alipay.com/a1",
	})
	app.accountPool.Add(&account.Account{
		ID: 2, Channel: "personal.alipay", Name: "附加账户", Status: account.StatusNormal,
		QRContent: "https://qr.alipay.com/a2",
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-match-1")
	if err := pdb.AddDeviceAccount(ctx, dev.ID, 2); err != nil {
		t.Fatalf("绑定附加账户失败: %v", err)
	}

	// 一笔 WAIT_PAY 订单（金额 100 分，账户 1）
	newTestOrder(t, app.store, "ORD-MATCH-1", 100, 1)

	// 账本新收款行：付款方 *怡 / 10:00:00 / +1.00（100分），30 分钟窗口内
	rows := []capture.AlipayLedgerRow{{
		Payer: "*怡", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+1.00", AmountMinor: 100,
	}}
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, rows); err != nil {
		t.Fatalf("dispatchLedgerRows 失败: %v", err)
	}

	// 订单应入账成功
	o, err := app.store.GetOrder(ctx, "ORD-MATCH-1")
	if err != nil || o == nil {
		t.Fatalf("查询订单失败: %v", err)
	}
	if o.Status != model.StatusSuccess {
		t.Fatalf("订单应 TRADE_SUCCESS，实际 %s", o.Status)
	}
	if o.DeviceID != dev.DeviceID {
		t.Errorf("订单 device_id 应回写，实际 %q", o.DeviceID)
	}

	// 推送流水应记录 matched（rawHash = sha256("ledger:*怡|HH:MM:SS|+1.00")）
	raw := "ledger:*怡|" + rows[0].Time + "|+1.00"
	rawHash := fmt.Sprintf("%x", sha256Hash([]byte(raw)))
	dup, err := pdb.PushExists(ctx, dev.DeviceID, rawHash)
	if err != nil || !dup {
		t.Fatalf("推送流水应存在（matched），err=%v dup=%v", err, dup)
	}
}

func TestDispatchLedgerRows_FrozenAccountGuard(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)

	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "冻结账户", Status: account.StatusFrozen,
		QRContent: "https://qr.alipay.com/f1",
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-frozen-1")
	newTestOrder(t, app.store, "ORD-FROZEN-1", 100, 1)

	rows := []capture.AlipayLedgerRow{{
		Payer: "*测", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+1.00", AmountMinor: 100,
	}}
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, rows); err != nil {
		t.Fatalf("dispatchLedgerRows 失败: %v", err)
	}

	// 冻结账户守卫：不自动入账，订单保持 WAIT_PAY
	o, err := app.store.GetOrder(ctx, "ORD-FROZEN-1")
	if err != nil || o == nil {
		t.Fatalf("查询订单失败: %v", err)
	}
	if o.Status != model.StatusWaitPay {
		t.Fatalf("冻结账户订单不应自动入账，实际 %s", o.Status)
	}
}

func TestMatchPersonalOrder_NoOrder(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-noorder-1")

	// 无匹配订单 → no_order
	orderNo, matched, acctID, err := app.matchPersonalOrder(ctx, dev, "alipay", "", 999, time.Now(), 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("matchPersonalOrder 失败: %v", err)
	}
	if matched != "no_order" || orderNo != "" || acctID != 0 {
		t.Fatalf("应 no_order，实际 matched=%q order=%q acct=%d", matched, orderNo, acctID)
	}
}

func TestMatchPersonalOrder_AmbiguousRefused(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-ambig-1")

	// 同金额两笔 WAIT_PAY 订单 → ambiguous（拒绝自动挑选）
	newTestOrder(t, app.store, "ORD-A1", 100, 1)
	newTestOrder(t, app.store, "ORD-A2", 100, 1)

	orderNo, matched, acctID, err := app.matchPersonalOrder(ctx, dev, "alipay", "", 100, time.Now(), 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("matchPersonalOrder 失败: %v", err)
	}
	if matched != "ambiguous" || orderNo != "" || acctID != 0 {
		t.Fatalf("同金额多订单应 ambiguous（资金安全），实际 matched=%q order=%q acct=%d", matched, orderNo, acctID)
	}
}

func TestDispatchLedgerRows_Dedup(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-dedup-1")
	newTestOrder(t, app.store, "ORD-DEDUP-1", 100, 1)

	// 同一行推两次：第二次应被幂等去重（不重复入账/不重复匹配）
	row := capture.AlipayLedgerRow{
		Payer: "*重", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+1.00", AmountMinor: 100,
	}
	rows := []capture.AlipayLedgerRow{row}
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, rows); err != nil {
		t.Fatalf("首次推送失败: %v", err)
	}
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, rows); err != nil {
		t.Fatalf("重复推送失败: %v", err)
	}

	// 订单已成功（首次入账），重复推送幂等
	o, err := app.store.GetOrder(ctx, "ORD-DEDUP-1")
	if err != nil || o == nil {
		t.Fatalf("查询订单失败: %v", err)
	}
	if o.Status != model.StatusSuccess {
		t.Fatalf("订单应 TRADE_SUCCESS，实际 %s", o.Status)
	}
}

// TestMatchPersonalOrder_NoteCode 备注码精确匹配（命中唯一订单直接入账，不做金额候选扩散）
func TestMatchPersonalOrder_NoteCode(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-note-1")

	// 备注码唯一订单（即使同金额有另一笔不同备注码订单，备注码应精确命中）
	newTestOrderNote(t, app.store, "ORD-N1", 100, 1, "abcd")
	newTestOrderNote(t, app.store, "ORD-N2", 100, 1, "efgh")
	// 池同步（等价于下单钩子）：两单都进池
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-N1", "ORD-N2"))

	orderNo, matched, acctID, err := app.matchPersonalOrder(ctx, dev, "alipay", "abcd", 0, time.Now(), 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("matchPersonalOrder 失败: %v", err)
	}
	if matched != "matched" || orderNo != "ORD-N1" || acctID != 1 {
		t.Fatalf("备注码应精确命中 ORD-N1，实际 matched=%q order=%q acct=%d", matched, orderNo, acctID)
	}
}

// TestMatchPersonalOrder_NoteCodePoolPath 池路径：备注码经内存池命中（零 DB 索引查询），
// DB 二次校验仍生效（池中订单已入账 → 池条目仍命中但 DB 状态非 WAIT_PAY → 不采信，走金额兜底）。
func TestMatchPersonalOrder_NoteCodePoolPath(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-note-pool-1")

	newTestOrderNote(t, app.store, "ORD-POOL-1", 100, 1, "wxyz")
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-POOL-1"))

	// 池路径命中
	orderNo, matched, acctID, err := app.matchPersonalOrder(ctx, dev, "alipay", "wxyz", 0, time.Now(), 30*time.Minute, 0)
	if err != nil || matched != "matched" || orderNo != "ORD-POOL-1" || acctID != 1 {
		t.Fatalf("池路径备注码应命中 ORD-POOL-1，实际 matched=%q order=%q acct=%d err=%v", matched, orderNo, acctID, err)
	}
	// 订单入账（SUCCESS）→ 池未移除时（模拟漂移），DB 二次校验应拒绝备注码采信
	if ok, serr := app.orders.SettlePayment(ctx, &channel.NotifyResult{
		OutTradeNo: "ORD-POOL-1", AmountMinor: 100, ChannelTxID: "TX-POOL-1",
	}); serr != nil || !ok {
		t.Fatalf("入账失败: ok=%v err=%v", ok, serr)
	}
	_, matched2, _, err2 := app.matchPersonalOrder(ctx, dev, "alipay", "wxyz", 0, time.Now(), 30*time.Minute, 0)
	if err2 != nil {
		t.Fatalf("匹配失败: %v", err2)
	}
	if matched2 == "matched" {
		t.Fatal("已入账订单备注码不应再采信（DB 二次校验拦截），实际仍 matched")
	}
}

// TestWaitPayPool_SyncWithOrderLifecycle 池与订单生命周期同步：下单进池 → 入账出池
func TestWaitPayPool_SyncWithOrderLifecycle(t *testing.T) {
	ctx := context.Background()
	app, _ := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	// 模拟下单（hooks 已注入）：DB 写订单 + 池 Add（与生产下单链路一致）
	o := &model.Order{
		OutTradeNo: "ORD-LIFE-1", GatewayNo: "G-ORD-LIFE-1", Channel: "personal.alipay", AccountID: 1,
		AmountMinor: 100, Status: model.StatusWaitPay, CreatedAt: time.Now(),
	}
	if err := app.store.CreateOrder(ctx, o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
	app.waitPayPool.Add(o)
	if got := app.waitPayPool.Count(1); got != 1 {
		t.Fatalf("下单后池应有 1 笔，实际 %d", got)
	}
	// 入账 → hooks Remove → 池出
	if ok, serr := app.orders.SettlePayment(ctx, &channel.NotifyResult{
		OutTradeNo: "ORD-LIFE-1", AmountMinor: 100, ChannelTxID: "TX-LIFE-1",
	}); serr != nil || !ok {
		t.Fatalf("入账失败: ok=%v err=%v", ok, serr)
	}
	if got := app.waitPayPool.Count(1); got != 0 {
		t.Fatalf("入账后池应清空该账户，实际 %d", got)
	}
}

// TestMatchPersonalOrder_ChannelMismatchRefused 通道归属校验：订单通道/账户不属于设备绑定集合 → 拒绝（防串单）
func TestMatchPersonalOrder_ChannelMismatchRefused(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	// 设备只绑定账户 1（支付宝）；账户 2 是微信（不在设备集合）
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	app.accountPool.Add(&account.Account{
		ID: 2, Channel: "personal.wechat", Name: "微信账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-chmismatch-1")

	// 同金额订单：一笔是微信通道（应被排除），一笔支付宝通道（应被选中）
	newTestOrderChannel(t, app.store, "ORD-WX", 100, 2, "personal.wechat")
	newTestOrderChannel(t, app.store, "ORD-ALI", 100, 1, "personal.alipay")

	orderNo, matched, acctID, err := app.matchPersonalOrder(ctx, dev, "alipay", "", 100, time.Now(), 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("matchPersonalOrder 失败: %v", err)
	}
	if matched != "matched" || orderNo != "ORD-ALI" || acctID != 1 {
		t.Fatalf("应命中支付宝订单 ORD-ALI（微信订单被通道校验排除），实际 matched=%q order=%q acct=%d", matched, orderNo, acctID)
	}
}

// newTestOrderChannel 创建指定通道的 WAIT_PAY 订单（防串单测试用）
func newTestOrderChannel(t *testing.T, st store.Store, outNo string, amountMinor int64, accountID int64, channel string) {
	t.Helper()
	o := &model.Order{
		OutTradeNo: outNo, GatewayNo: "G-" + outNo, Channel: channel,
		AccountID: accountID, AmountMinor: amountMinor, Status: model.StatusWaitPay,
		CreatedAt: time.Now(),
	}
	if err := st.CreateOrder(context.Background(), o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}
}

// mustOrders 按 outTradeNo 列表从 store 取订单（池重建测试用；取不到直接失败）
func mustOrders(t *testing.T, st store.Store, outNos ...string) []*model.Order {
	t.Helper()
	out := make([]*model.Order, 0, len(outNos))
	for _, no := range outNos {
		o, err := st.GetOrder(context.Background(), no)
		if err != nil || o == nil {
			t.Fatalf("查询订单 %s 失败: %v", no, err)
		}
		out = append(out, o)
	}
	return out
}

// stubNoteProber 构造备注码探测桩：固定返回 note（探测成功）/ 固定错误（探测失败）
func stubNoteProber(note string, err error) alipayNoteProber {
	return func(ctx context.Context, dev *personal.Device, bounds string) (string, error) {
		return note, err
	}
}

// TestMatchLedgerRow_ProbeDisambiguates 多候选歧义 → 探测详情页备注码消歧 → 精确命中唯一订单。
// 池消费模型核心场景：同金额两笔订单，备注码探测区分归属，不再转人工。
func TestMatchLedgerRow_ProbeDisambiguates(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
		QRContent: "https://qr.alipay.com/a1",
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-probe-1")

	// 同金额两笔 WAIT_PAY 订单，备注码不同（jzt4 / 7x57）
	newTestOrderNote(t, app.store, "ORD-PROBE-A", 100, 1, "jzt4")
	newTestOrderNote(t, app.store, "ORD-PROBE-B", 100, 1, "7x57")
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-PROBE-A", "ORD-PROBE-B"))

	row := capture.AlipayLedgerRow{
		Payer: "*怡", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+1.00", AmountMinor: 100, AmountBounds: "[451,374][1013,470]",
	}
	orderNo, matched, acctID, effNote, err := app.matchLedgerRow(ctx, dev, "alipay", 0, row, 30*time.Minute, stubNoteProber("jzt4", nil))
	if err != nil {
		t.Fatalf("matchLedgerRow 失败: %v", err)
	}
	if matched != "matched" || orderNo != "ORD-PROBE-A" || acctID != 1 {
		t.Fatalf("探测应消歧命中 ORD-PROBE-A，实际 matched=%q order=%q acct=%d", matched, orderNo, acctID)
	}
	if effNote != "jzt4" {
		t.Errorf("审计备注码应为探测到的 jzt4，实际 %q", effNote)
	}
}

// TestMatchLedgerRow_ProbeFailsStaysAmbiguous 探测失败 → 保持 ambiguous 转人工（资金安全：禁止自动挑选）。
func TestMatchLedgerRow_ProbeFailsStaysAmbiguous(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-probe-fail-1")

	newTestOrder(t, app.store, "ORD-PF-A", 100, 1)
	newTestOrder(t, app.store, "ORD-PF-B", 100, 1)
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-PF-A", "ORD-PF-B"))

	row := capture.AlipayLedgerRow{
		Payer: "*怡", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+1.00", AmountMinor: 100, AmountBounds: "[451,374][1013,470]",
	}
	orderNo, matched, _, _, err := app.matchLedgerRow(ctx, dev, "alipay", 0, row, 30*time.Minute, stubNoteProber("", fmt.Errorf("adb 不可达")))
	if err != nil {
		t.Fatalf("matchLedgerRow 不应因探测失败报错（降级歧义转人工）: %v", err)
	}
	if matched != "ambiguous" || orderNo != "" {
		t.Fatalf("探测失败应保持 ambiguous，实际 matched=%q order=%q", matched, orderNo)
	}
}

// TestMatchLedgerRow_ProbeEmptyNoteStaysAmbiguous 探测成功但无备注码（详情页无付款留言）→ 保持 ambiguous。
func TestMatchLedgerRow_ProbeEmptyNoteStaysAmbiguous(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-probe-empty-1")

	newTestOrder(t, app.store, "ORD-PE-A", 100, 1)
	newTestOrder(t, app.store, "ORD-PE-B", 100, 1)
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-PE-A", "ORD-PE-B"))

	row := capture.AlipayLedgerRow{
		Payer: "*怡", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+1.00", AmountMinor: 100, AmountBounds: "[451,374][1013,470]",
	}
	orderNo, matched, _, _, err := app.matchLedgerRow(ctx, dev, "alipay", 0, row, 30*time.Minute, stubNoteProber("", nil))
	if err != nil {
		t.Fatalf("matchLedgerRow 失败: %v", err)
	}
	if matched != "ambiguous" || orderNo != "" {
		t.Fatalf("空备注码应保持 ambiguous，实际 matched=%q order=%q", matched, orderNo)
	}
}

// TestMatchLedgerRow_UniqueAmountSkipsProbe 金额唯一 → 直接命中，探测桩不应被调用（短路免探测优化）。
func TestMatchLedgerRow_UniqueAmountSkipsProbe(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-unique-1")

	newTestOrder(t, app.store, "ORD-UNIQ-1", 100, 1)
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-UNIQ-1"))

	probeCalled := false
	prober := func(ctx context.Context, dev *personal.Device, bounds string) (string, error) {
		probeCalled = true
		return "", fmt.Errorf("不应被调用")
	}
	row := capture.AlipayLedgerRow{
		Payer: "*怡", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+1.00", AmountMinor: 100, AmountBounds: "[451,374][1013,470]",
	}
	orderNo, matched, _, _, err := app.matchLedgerRow(ctx, dev, "alipay", 0, row, 30*time.Minute, prober)
	if err != nil {
		t.Fatalf("matchLedgerRow 失败: %v", err)
	}
	if matched != "matched" || orderNo != "ORD-UNIQ-1" {
		t.Fatalf("金额唯一应直接命中，实际 matched=%q order=%q", matched, orderNo)
	}
	if probeCalled {
		t.Error("金额唯一时不应触发备注码探测（短路免探测）")
	}
}

// TestMatchLedgerRow_WechatRowNoteDirect 微信账本行自带"付款方备注" → 备注码精确命中，无需探测。
func TestMatchLedgerRow_WechatRowNoteDirect(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 2, Channel: "personal.wechat", Name: "微信账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 2, "dev-wx-note-1")

	// 微信订单备注码直接入库（与支付宝一致的 note_code 机制）
	if err := app.store.CreateOrder(ctx, &model.Order{
		OutTradeNo: "ORD-WXNOTE-1", GatewayNo: "G-ORD-WXNOTE-1", Channel: "personal.wechat",
		AccountID: 2, AmountMinor: 100, Status: model.StatusWaitPay,
		NoteCode: "wxcode", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("创建微信订单失败: %v", err)
	}
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-WXNOTE-1"))

	row := capture.AlipayLedgerRow{
		Payer: "*微", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+1.00", AmountMinor: 100, Note: "wxcode", // 微信账本行自带备注
	}
	orderNo, matched, _, effNote, err := app.matchLedgerRow(ctx, dev, "wechat", 0, row, 30*time.Minute, nil)
	if err != nil {
		t.Fatalf("matchLedgerRow 失败: %v", err)
	}
	if matched != "matched" || orderNo != "ORD-WXNOTE-1" {
		t.Fatalf("微信行备注应精确命中，实际 matched=%q order=%q", matched, orderNo)
	}
	if effNote != "wxcode" {
		t.Errorf("审计备注码应为 wxcode，实际 %q", effNote)
	}
}

// TestDispatchLedgerRows_ProbeThroughDispatch 全链路：dispatchLedgerRows 多候选歧义 → 探测消歧 → 入账。
// 通过临时替换 app 的探测函数注入桩（生产路径 = adb 探测，测试 = 桩）。
func TestDispatchLedgerRows_ProbeThroughDispatch(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
		QRContent: "https://qr.alipay.com/a1",
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-dispatch-probe-1")

	newTestOrderNote(t, app.store, "ORD-DPROBE-A", 100, 1, "jzt4")
	newTestOrderNote(t, app.store, "ORD-DPROBE-B", 100, 1, "7x57")
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-DPROBE-A", "ORD-DPROBE-B"))

	// 注入探测桩
	app.alipayProber = stubNoteProber("jzt4", nil)
	t.Cleanup(func() { app.alipayProber = nil })

	rows := []capture.AlipayLedgerRow{{
		Payer: "*怡", Date: "今日", Time: time.Now().Format("15:04:05"),
		Amount: "+1.00", AmountMinor: 100, AmountBounds: "[451,374][1013,470]",
	}}
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, rows); err != nil {
		t.Fatalf("dispatchLedgerRows 失败: %v", err)
	}

	o, err := app.store.GetOrder(ctx, "ORD-DPROBE-A")
	if err != nil || o == nil {
		t.Fatalf("查询订单失败: %v", err)
	}
	if o.Status != model.StatusSuccess {
		t.Fatalf("探测消歧订单应入账成功，实际 %s", o.Status)
	}
	// 另一笔保持 WAIT_PAY（未被误配）
	oB, err := app.store.GetOrder(ctx, "ORD-DPROBE-B")
	if err != nil || oB == nil {
		t.Fatalf("查询订单失败: %v", err)
	}
	if oB.Status != model.StatusWaitPay {
		t.Fatalf("未命中订单应保持 WAIT_PAY，实际 %s", oB.Status)
	}
}

// TestDispatchLedgerRows_MultiRowsNoMiss 多行一次推送全部处理（不漏行）：每笔金额独立匹配池中订单。
func TestDispatchLedgerRows_MultiRowsNoMiss(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-multi-1")

	newTestOrder(t, app.store, "ORD-M1", 100, 1)
	newTestOrder(t, app.store, "ORD-M2", 200, 1)
	newTestOrder(t, app.store, "ORD-M3", 300, 1)
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-M1", "ORD-M2", "ORD-M3"))

	now := time.Now().Format("15:04:05")
	rows := []capture.AlipayLedgerRow{
		{Payer: "*甲", Date: "今日", Time: now, Amount: "+1.00", AmountMinor: 100},
		{Payer: "*乙", Date: "今日", Time: now, Amount: "+2.00", AmountMinor: 200},
		{Payer: "*丙", Date: "今日", Time: now, Amount: "+3.00", AmountMinor: 300},
	}
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, rows); err != nil {
		t.Fatalf("dispatchLedgerRows 失败: %v", err)
	}

	for _, no := range []string{"ORD-M1", "ORD-M2", "ORD-M3"} {
		o, err := app.store.GetOrder(ctx, no)
		if err != nil || o == nil {
			t.Fatalf("查询订单 %s 失败: %v", no, err)
		}
		if o.Status != model.StatusSuccess {
			t.Errorf("多行推送不应漏单：%s 应入账成功，实际 %s", no, o.Status)
		}
	}
}

// TestDispatchLedgerRows_ZeroCandidateNoMiss 零候选行不阻断后续行（不漏行：歧义/无单行独立转人工）。
func TestDispatchLedgerRows_ZeroCandidateNoMiss(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-zero-1")

	// 只有 100 分订单；推 100（命中）+ 999（零候选）+ 100 重复（幂等去重）
	newTestOrder(t, app.store, "ORD-Z1", 100, 1)
	app.waitPayPool.Rebuild(ctx, mustOrders(t, app.store, "ORD-Z1"))

	now := time.Now().Format("15:04:05")
	rows := []capture.AlipayLedgerRow{
		{Payer: "*甲", Date: "今日", Time: now, Amount: "+1.00", AmountMinor: 100},
		{Payer: "*零", Date: "今日", Time: now, Amount: "+9.99", AmountMinor: 999},
	}
	if err := app.dispatchLedgerRows(ctx, dev.DeviceID, "alipay", 0, rows); err != nil {
		t.Fatalf("dispatchLedgerRows 失败: %v", err)
	}

	// 命中行入账
	o, err := app.store.GetOrder(ctx, "ORD-Z1")
	if err != nil || o == nil {
		t.Fatalf("查询订单失败: %v", err)
	}
	if o.Status != model.StatusSuccess {
		t.Fatalf("零候选行不应影响命中行入账，实际 %s", o.Status)
	}
	// 零候选行应转人工（unmatched 记录）
	unmatched, err := pdb.ListUnmatched(ctx, "pending", 10)
	if err != nil {
		t.Fatalf("查询未匹配记录失败: %v", err)
	}
	found := false
	for _, u := range unmatched {
		if u.AmountMinor == 999 {
			found = true
			break
		}
	}
	if !found {
		t.Error("零候选行应记录到 unmatched_payments（转人工），未找到 999 分记录")
	}
}

// TestMatchPersonalOrder_NotifyAnchor 通知栏窗口锚点语义（2026-08-30 用户需求确认）：
// 通知不清理会一直在通知栏，匹配窗口必须以通知 when 为锚，不能用 time.Now()。
// 场景：新订单刚创建，但旧通知（when 在 2 小时前）不应匹配到它。
func TestMatchPersonalOrder_NotifyAnchor(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
		QRContent: "https://qr.alipay.com/new-notify-anchor",
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-notify-anchor")
	// 设备绑定空间0的收款码（让 space 账户定位生效，走同一链路）
	_, _ = pdb.UpsertQrcode(ctx, &personal.DeviceQrcode{
		DeviceID: dev.DeviceID, Platform: "alipay", UserID: 0,
		QRContent: "https://qr.alipay.com/new-notify-anchor", Status: "active",
	})
	// 创建一笔 WAIT_PAY 订单（刚创建）
	newTestOrder(t, app.store, "ORD-NOTIFY-NEW", 500, 1)

	// 旧通知：when 在 2 小时前（通知栏一直没清）
	oldNotify := time.Now().Add(-2 * time.Hour)
	orderNo, matched, _, err := app.matchPersonalOrder(ctx, dev, "alipay", "", 500, oldNotify, 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("matchPersonalOrder 失败: %v", err)
	}
	if matched != "no_order" || orderNo != "" {
		t.Fatalf("旧通知不应匹配新订单：matched=%q order=%q（期望 no_order）", matched, orderNo)
	}

	// 对照：通知 when = 刚刚（新通知）→ 锚点窗口覆盖订单 → 应命中
	newNotify := time.Now()
	orderNo2, matched2, _, err := app.matchPersonalOrder(ctx, dev, "alipay", "", 500, newNotify, 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("matchPersonalOrder 失败: %v", err)
	}
	if matched2 != "matched" || orderNo2 != "ORD-NOTIFY-NEW" {
		t.Fatalf("新通知应匹配新订单：matched=%q order=%q（期望 matched ORD-NOTIFY-NEW）", matched2, orderNo2)
	}
}

// 双链同捕竞态：通知栏/账本 5s 内先后捕获同一笔收款，胜者已把订单结算，
// 败者匹配时零 WAIT_PAY 候选 → 应判定 settled_dup（重复捕获），不得误报 no_order 入人工池。
func TestMatchPersonalOrder_SettledDup(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-dup-1")

	newTestOrder(t, app.store, "ORD-DUP-1", 100, 1)
	if _, err := app.store.UpdateOrderStatus(ctx, "ORD-DUP-1", model.StatusWaitPay, model.StatusSuccess,
		func(upd *model.Order) { upd.ChannelTxID = "LEDGER-x" }); err != nil {
		t.Fatalf("模拟另一链路入账失败: %v", err)
	}

	orderNo, matched, acctID, err := app.matchPersonalOrder(ctx, dev, "alipay", "", 100, time.Now(), 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("matchPersonalOrder 失败: %v", err)
	}
	if matched != "settled_dup" || orderNo != "ORD-DUP-1" || acctID != 1 {
		t.Fatalf("应 settled_dup，实际 matched=%q order=%q acct=%d", matched, orderNo, acctID)
	}
}

// 窗口内同金额已结算订单多于 1 笔 → 无法归属，保持 no_order（资金安全，不猜）。
func TestMatchPersonalOrder_SettledDupAmbiguous(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.alipay", Name: "主账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-dup-2")

	newTestOrder(t, app.store, "ORD-DUP-A", 100, 1)
	newTestOrder(t, app.store, "ORD-DUP-B", 100, 1)
	for _, no := range []struct{ id, tx string }{{"ORD-DUP-A", "LEDGER-a"}, {"ORD-DUP-B", "LEDGER-b"}} {
		if _, err := app.store.UpdateOrderStatus(ctx, no.id, model.StatusWaitPay, model.StatusSuccess,
			func(upd *model.Order) { upd.ChannelTxID = no.tx }); err != nil {
			t.Fatalf("模拟入账失败: %v", err)
		}
	}

	orderNo, matched, _, err := app.matchPersonalOrder(ctx, dev, "alipay", "", 100, time.Now(), 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("matchPersonalOrder 失败: %v", err)
	}
	if matched != "no_order" || orderNo != "" {
		t.Fatalf("多笔已结算不可归属，应 no_order，实际 matched=%q order=%q", matched, orderNo)
	}
}

// 通道校验同样约束 settled_dup：已结算订单的通道不在设备绑定集合 → no_order。
func TestMatchPersonalOrder_SettledDupWrongChannel(t *testing.T) {
	ctx := context.Background()
	app, pdb := newMatchTestEnv(t)
	app.accountPool.Add(&account.Account{
		ID: 1, Channel: "personal.wechat", Name: "微信账户", Status: account.StatusNormal,
	})
	dev := newTestDevice(t, pdb, app.accountPool, 1, "dev-dup-3")

	newTestOrder(t, app.store, "ORD-DUP-W", 100, 1)
	if _, err := app.store.UpdateOrderStatus(ctx, "ORD-DUP-W", model.StatusWaitPay, model.StatusSuccess,
		func(upd *model.Order) { upd.ChannelTxID = "LEDGER-x" }); err != nil {
		t.Fatalf("模拟入账失败: %v", err)
	}

	orderNo, matched, _, err := app.matchPersonalOrder(ctx, dev, "alipay", "", 100, time.Now(), 30*time.Minute, 0)
	if err != nil {
		t.Fatalf("matchPersonalOrder 失败: %v", err)
	}
	if matched != "no_order" || orderNo != "" {
		t.Fatalf("通道不符不应归属，实际 matched=%q order=%q", matched, orderNo)
	}
}
