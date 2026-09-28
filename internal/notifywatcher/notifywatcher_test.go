package notifywatcher

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeADB 注入 adb Shell 输出的桩（实现 Sheller 接口）。
type fakeADB struct {
	mu    sync.Mutex
	calls int
	out   string
}

func (f *fakeADB) Shell(ctx context.Context, timeout time.Duration, serial, command string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.out, nil
}

// fakeSeen 去重存储桩（实现 SeenStore 接口）。
type fakeSeen struct {
	dupe bool
}

func (f *fakeSeen) PushExists(ctx context.Context, deviceID, rawHash string) (bool, error) {
	return f.dupe, nil
}

func TestNewDefaults(t *testing.T) {
	m := New(nil, nil, nil, nil, nil, Config{})
	if m.cfg.PollEvery != defaultPollEvery {
		t.Errorf("PollEvery = %v, 期望默认 %v", m.cfg.PollEvery, defaultPollEvery)
	}
	if m.cfg.IdleEvery != defaultIdleEvery {
		t.Errorf("IdleEvery = %v, 期望默认 %v", m.cfg.IdleEvery, defaultIdleEvery)
	}
	if m.cfg.ShellTimeout != defaultShellTimeout {
		t.Errorf("ShellTimeout = %v, 期望默认 %v", m.cfg.ShellTimeout, defaultShellTimeout)
	}
}

func TestPollOnce_CallsDumpAndHandler(t *testing.T) {
	adb := &fakeADB{out: realWechatMain}
	var mu sync.Mutex
	handled := 0
	m := New(adb, func(ctx context.Context) ([]Device, error) {
		return []Device{{DeviceID: "dev-01", Serial: "1.2.3.4:5555"}}, nil
	}, nil, func(ctx context.Context, ev Event) error {
		mu.Lock()
		defer mu.Unlock()
		handled++
		if ev.Provider != "wechat" || ev.AmountMinor != 10 {
			t.Errorf("回调参数异常: %+v", ev)
		}
		return nil
	}, &fakeSeen{}, Config{})

	m.pollOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if handled != 1 {
		t.Errorf("期望回调 1 次，实际 %d", handled)
	}
}

func TestPollOnce_Dedup(t *testing.T) {
	adb := &fakeADB{out: realWechatMain}
	handled := 0
	m := New(adb, func(ctx context.Context) ([]Device, error) {
		return []Device{{DeviceID: "dev-01", Serial: "s1"}}, nil
	}, nil, func(ctx context.Context, ev Event) error {
		handled++
		return nil
	}, &fakeSeen{dupe: true}, Config{})

	m.pollOnce(context.Background())
	if handled != 0 {
		t.Errorf("已去重通知不应回调，实际 %d 次", handled)
	}
}

func TestPollOnce_MultipleEvents(t *testing.T) {
	adb := &fakeADB{out: realWechatMain + "\n" + realAlipay}
	handled := make(map[string]int)
	var mu sync.Mutex
	m := New(adb, func(ctx context.Context) ([]Device, error) {
		return []Device{{DeviceID: "dev-01", Serial: "s1"}}, nil
	}, nil, func(ctx context.Context, ev Event) error {
		mu.Lock()
		defer mu.Unlock()
		handled[ev.Provider]++
		return nil
	}, &fakeSeen{}, Config{})

	m.pollOnce(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if handled["wechat"] != 1 || handled["alipay"] != 1 {
		t.Errorf("期望微信/支付宝各 1 次，实际 %+v", handled)
	}
}

func TestPollOnce_HandlerError(t *testing.T) {
	adb := &fakeADB{out: realWechatMain}
	calls := 0
	m := New(adb, func(ctx context.Context) ([]Device, error) {
		return []Device{{DeviceID: "dev-01", Serial: "s1"}}, nil
	}, nil, func(ctx context.Context, ev Event) error {
		calls++
		return context.DeadlineExceeded
	}, &fakeSeen{}, Config{})

	m.pollOnce(context.Background())
	if calls != 1 {
		t.Errorf("回调应被调用 1 次，实际 %d", calls)
	}
}

func TestStart_OrderDrivenInterval(t *testing.T) {
	// 有设备枚举测量轮询频率（adb.calls 反映实际 dump 次数）
	adb := &fakeADB{out: ""}
	var orderDriven atomic.Bool
	orderDriven.Store(true)
	hasPend := func(ctx context.Context) bool { return orderDriven.Load() }

	m := New(adb, func(ctx context.Context) ([]Device, error) {
		return []Device{{DeviceID: "dev-01", Serial: "s1"}}, nil
	}, hasPend, func(ctx context.Context, ev Event) error { return nil },
		nil, Config{PollEvery: 30 * time.Millisecond, IdleEvery: 200 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	go m.Start(ctx)

	time.Sleep(400 * time.Millisecond)
	adb.mu.Lock()
	activeCalls := adb.calls
	adb.mu.Unlock()

	// 转池空 → 完全停止轮询（事件驱动休眠，仅 idle 兜底 200ms 一次）
	orderDriven.Store(false)
	time.Sleep(400 * time.Millisecond)
	adb.mu.Lock()
	afterCalls := adb.calls
	adb.mu.Unlock()

	// 事件唤醒：真实时序 = 订单先入池（hasPend 变 true）再 Wake → 休眠态立即醒来轮询
	orderDriven.Store(true) // 订单入池
	m.Wake()                // 通知栏监听被唤醒
	time.Sleep(150 * time.Millisecond)
	adb.mu.Lock()
	wakeAfter := adb.calls
	adb.mu.Unlock()

	cancel()
	time.Sleep(50 * time.Millisecond)

	if activeCalls < 5 {
		t.Errorf("有订单阶段 dump 过少: %d（期望高频 ≥5）", activeCalls)
	}
	// 池空 400ms：idle 兜底 200ms 一次 → 最多 2-3 次（事件驱动下基本停止）
	if afterCalls-activeCalls > 4 {
		t.Errorf("池空阶段 dump 过多: %d（期望休眠低频）", afterCalls-activeCalls)
	}
	// 订单入池 + Wake 后：150ms 内应恢复高频轮询（事件驱动即时响应）
	if wakeAfter-activeCalls < 1 {
		t.Errorf("Wake 后未恢复轮询: before=%d after=%d", activeCalls, wakeAfter)
	}
}

func TestStart_Cancel(t *testing.T) {
	adb := &fakeADB{out: ""}
	m := New(adb, func(ctx context.Context) ([]Device, error) {
		return []Device{{DeviceID: "dev-01", Serial: "s1"}}, nil
	}, nil, func(ctx context.Context, ev Event) error { return nil },
		nil, Config{PollEvery: 10 * time.Millisecond, IdleEvery: 10 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Start(ctx)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("Start 未在 cancel 后退出")
	}
}
