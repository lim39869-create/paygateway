package reconciler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"paygateway/internal/account"
	"paygateway/internal/ledger"
	"paygateway/internal/model"
	"paygateway/internal/order"
	"paygateway/internal/personal"
	"paygateway/internal/store"
	"paygateway/internal/upstream"
)

// newTestEnv 构建测试环境：临时业务库 + personal.db + 账户池 + 订单服务
func newTestEnv(t *testing.T) (st store.Store, pdb *store.PersonalDB, pool *account.AccountPool, svc *order.Service, dir string) {
	t.Helper()
	dir = t.TempDir()

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

	pdb, err = store.NewPersonalDB(filepath.Join(dir, "personal.db"))
	if err != nil {
		t.Fatalf("创建个人库失败: %v", err)
	}
	t.Cleanup(func() { pdb.Close() })

	pool = account.NewPool()
	lg, err := ledger.New(ldb, "")
	if err != nil {
		t.Fatalf("创建账本失败: %v", err)
	}
	svc = order.New(st, lg, pool, upstream.NewPool())

	// 账户池加一个个人免签账户
	acct := &account.Account{
		ID: 1, Channel: "personal.alipay", Name: "测试账户", Status: account.StatusNormal,
		QRContent: "https://qr.alipay.com/test123", DailyLimit: 100000,
	}
	pool.Add(acct)
	return st, pdb, pool, svc, dir
}

// TestDeviceWatcherOffline 核心场景：adb 不可达 → 设备 OFFLINE → 账户冻结 → 订单进异常池
func TestDeviceWatcherOffline(t *testing.T) {
	ctx := context.Background()
	st, pdb, pool, svc, _ := newTestEnv(t)

	// 1. 创建设备（绑定账户1，status=online，但无 adb serial → adb 不可达）
	devID, err := pdb.CreateDevice(ctx, &personal.DeviceEncrypted{
		DeviceID: "dev-offline-1", Name: "离线测试机", AccountID: 1,
		Provider: "alipay", SecretEnc: "xx", Status: string(personal.DeviceOnline),
	})
	if err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	// 2. 创建 WAIT_PAY 订单（account_id=1）
	o := &model.Order{
		OutTradeNo: "T-ORDER-001", GatewayNo: "G-001", Channel: "personal.alipay",
		AccountID: 1, AmountMinor: 100, Status: model.StatusWaitPay,
		CreatedAt: time.Now(),
	}
	if err := st.CreateOrder(ctx, o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	// 3. 运行离线扫描（adb 全程不可达：连续两次扫描确认判离线）
	w := NewDeviceWatcher(pdb, pool, st, svc, &DeviceWatcherConfig{
		ScanInterval: time.Second,
	}, nil, mockAdbProber{online: map[string]bool{}})
	w.scan(ctx)

	// 3.5 单次不可达不应立即判离线（双保险防抖动）
	dev0, _ := pdb.GetDevice(ctx, "dev-offline-1")
	if dev0 == nil || dev0.Status != string(personal.DeviceOnline) {
		t.Fatalf("单次不可达不应置 OFFLINE（需连续确认），实际 status=%v", dev0.Status)
	}
	if acct0 := pool.GetByID(1); acct0.Status != account.StatusNormal {
		t.Fatalf("单次不可达不应冻结账户，实际 status=%v", acct0.Status)
	}

	// 3.6 连续第二次扫描 → 判离线
	w.scan(ctx)

	// 4. 断言：设备 OFFLINE
	dev, _ := pdb.GetDevice(ctx, "dev-offline-1")
	if dev == nil || dev.Status != string(personal.DeviceOffline) {
		t.Fatalf("设备应置 OFFLINE，实际 status=%v", dev.Status)
	}

	// 5. 断言：账户冻结
	acct := pool.GetByID(1)
	if acct.Status != account.StatusFrozen {
		t.Fatalf("账户应冻结，实际 status=%v", acct.Status)
	}

	// 6. 断言：订单进异常池（reason=listen_offline）
	exc, err := st.GetExceptionByOutTradeNo(ctx, "T-ORDER-001")
	if err != nil || exc == nil {
		t.Fatalf("订单应进异常池，err=%v exc=%v", err, exc)
	}
	if exc.Reason != model.ExReasonListenOffline {
		t.Fatalf("异常原因应为 listen_offline，实际 %s", exc.Reason)
	}

	// 7. 幂等：再次扫描不重复进池
	w.scan(ctx)
	exc2, _ := st.GetExceptionByOutTradeNo(ctx, "T-ORDER-001")
	if exc2.ID != exc.ID {
		t.Fatal("重复扫描不应创建重复异常单")
	}

	_ = devID
}

// TestDeviceWatcherOnline adb 可达 → 不触发任何联动
func TestDeviceWatcherOnline(t *testing.T) {
	ctx := context.Background()
	st, pdb, pool, svc, _ := newTestEnv(t)

	// 设备配了 WiFi adb 且可达 → 在线
	if _, err := pdb.CreateDevice(ctx, &personal.DeviceEncrypted{
		DeviceID: "dev-online-1", Name: "在线测试机", AccountID: 1,
		Provider: "alipay", SecretEnc: "xx", Status: string(personal.DeviceOnline),
		AdbWifiHost: "192.168.1.50", AdbWifiPort: 5555,
	}); err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}
	o := &model.Order{
		OutTradeNo: "T-ORDER-002", GatewayNo: "G-002", Channel: "personal.alipay",
		AccountID: 1, AmountMinor: 200, Status: model.StatusWaitPay,
		CreatedAt: time.Now(),
	}
	if err := st.CreateOrder(ctx, o); err != nil {
		t.Fatalf("创建订单失败: %v", err)
	}

	w := NewDeviceWatcher(pdb, pool, st, svc, nil, nil, mockAdbProber{
		online: map[string]bool{"192.168.1.50:5555": true},
	})
	w.scan(ctx)

	// 设备仍在线、账户仍 normal、订单未进池
	dev, _ := pdb.GetDevice(ctx, "dev-online-1")
	if dev.Status != string(personal.DeviceOnline) {
		t.Fatalf("设备应保持在线，实际 %s", dev.Status)
	}
	if acct := pool.GetByID(1); acct.Status != account.StatusNormal {
		t.Fatalf("账户应保持 normal，实际 %s", acct.Status)
	}
	if exc, _ := st.GetExceptionByOutTradeNo(ctx, "T-ORDER-002"); exc != nil {
		t.Fatal("在线设备订单不应进异常池")
	}
}

// TestDeviceWatcherFrozen 手动冻结设备不被自动置离线
func TestDeviceWatcherFrozen(t *testing.T) {
	ctx := context.Background()
	st, pdb, pool, svc, _ := newTestEnv(t)

	// 冻结设备（即使 adb 不可达也不重复处理）
	if _, err := pdb.CreateDevice(ctx, &personal.DeviceEncrypted{
		DeviceID: "dev-frozen-1", Name: "冻结机", AccountID: 1,
		Provider: "alipay", SecretEnc: "xx", Status: string(personal.DeviceFrozen),
	}); err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	w := NewDeviceWatcher(pdb, pool, st, svc, nil, nil, mockAdbProber{online: map[string]bool{}})
	w.scan(ctx)

	dev, _ := pdb.GetDevice(ctx, "dev-frozen-1")
	if dev.Status != string(personal.DeviceFrozen) {
		t.Fatalf("冻结设备不应被改变，实际 %s", dev.Status)
	}
	if acct := pool.GetByID(1); acct.Status != account.StatusNormal {
		t.Fatal("冻结设备不应触发账户冻结（账户冻结由设备离线联动触发，冻结设备不在线）")
	}
}

// mockAdbProber 测试用 AdbProber：serial → 可达性
type mockAdbProber struct {
	online map[string]bool
}

func (m mockAdbProber) Online(_ context.Context, serial string) bool { return m.online[serial] }
func (mockAdbProber) Connect(context.Context, string) error          { return nil }

// TestDeviceWatcherAdbRecover 设备 offline + adb 恢复 → 自动置 online + 解冻离线联动账户
func TestDeviceWatcherAdbRecover(t *testing.T) {
	ctx := context.Background()
	st, pdb, pool, svc, _ := newTestEnv(t)

	// 设备 offline（此前离线联动冻结了账户），配了 WiFi adb
	if _, err := pdb.CreateDevice(ctx, &personal.DeviceEncrypted{
		DeviceID: "dev-recover-1", Name: "恢复测试机", AccountID: 1,
		Provider: "alipay", SecretEnc: "xx", Status: string(personal.DeviceOffline),
		AdbWifiHost: "192.168.1.50", AdbWifiPort: 5555,
	}); err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}
	// 账户处于离线联动冻结态（模拟 handleOffline 后的状态）
	pool.FreezeByOffline(1)

	w := NewDeviceWatcher(pdb, pool, st, svc, nil, nil, mockAdbProber{
		online: map[string]bool{"192.168.1.50:5555": true},
	})
	w.scan(ctx)

	dev, _ := pdb.GetDevice(ctx, "dev-recover-1")
	if dev.Status != string(personal.DeviceOnline) {
		t.Fatalf("adb 恢复后设备应置 online，实际 %s", dev.Status)
	}
	if acct := pool.GetByID(1); acct.Status != account.StatusNormal {
		t.Fatalf("离线联动冻结的账户应在设备恢复后自动解冻，实际 %s", acct.Status)
	}
}

// TestDeviceWatcherManualFrozenNotAutoUnfreeze 手动冻结的账户绝不被设备 adb 恢复自动解冻（资金安全底线）
func TestDeviceWatcherManualFrozenNotAutoUnfreeze(t *testing.T) {
	ctx := context.Background()
	st, pdb, pool, svc, _ := newTestEnv(t)

	// 设备 offline + adb 可达（会触发 recoverOnline）
	if _, err := pdb.CreateDevice(ctx, &personal.DeviceEncrypted{
		DeviceID: "dev-manual-1", Name: "手动冻结测试机", AccountID: 1,
		Provider: "alipay", SecretEnc: "xx", Status: string(personal.DeviceOffline),
		AdbHost: "4f2bdc30",
	}); err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}
	// 账户是「手动冻结」（不是离线联动）——模拟管理员 handleFreezeAccount 后的状态
	pool.FreezeManual(1)

	w := NewDeviceWatcher(pdb, pool, st, svc, nil, nil, mockAdbProber{
		online: map[string]bool{"4f2bdc30": true},
	})
	w.scan(ctx)

	// 设备可恢复 online，但手动冻结的账户必须保持 frozen
	dev, _ := pdb.GetDevice(ctx, "dev-manual-1")
	if dev.Status != string(personal.DeviceOnline) {
		t.Fatalf("设备应恢复 online，实际 %s", dev.Status)
	}
	if acct := pool.GetByID(1); acct.Status != account.StatusFrozen {
		t.Fatalf("手动冻结的账户绝不能被自动解冻，实际 %s", acct.Status)
	}
	if pool.IsOfflineFrozen(1) {
		t.Fatal("手动冻结账户不应标记为 offlineFrozen")
	}
}
