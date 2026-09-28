package main

import (
	"testing"
	"time"

	"paygateway/internal/model"
	"paygateway/internal/pool"
)

// TestPersonalOrderCloseAt_StaticFallback 静态关单模式（无动态窗口）：使用 ORDER_CLOSE_MINUTES。
func TestPersonalOrderCloseAt_StaticFallback(t *testing.T) {
	app := &App{cfg: &Config{OrderCloseMinutes: 30}}
	now := time.Now()
	o := &model.Order{OutTradeNo: "X", CreatedAt: now, Channel: "personal.alipay"}
	got := app.personalOrderCloseAt(o)
	want := now.Add(30 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("静态窗口下应 = CreatedAt+30min，实际 %v want %v", got, want)
	}
}

// TestPersonalOrderCloseAt_DynamicNotOverrideStatic 动态窗口已启用：个人订单回退用动态下限 ORDER_CLOSE_MIN，
// 绝不落静态 ORDER_CLOSE_MINUTES（防止环境配置拉长个人订单关单——幽灵订单根因之一）。
func TestPersonalOrderCloseAt_DynamicNotOverrideStatic(t *testing.T) {
	app := &App{cfg: &Config{OrderCloseMinutes: 30, OrderCloseMin: 5, OrderCloseMax: 10}}
	now := time.Now()
	o := &model.Order{OutTradeNo: "X", CreatedAt: now, Channel: "personal.wechat"}
	got := app.personalOrderCloseAt(o)
	want := now.Add(5 * time.Minute)
	if !got.Equal(want) {
		t.Fatalf("动态窗口下个人订单应 = CreatedAt+ORDER_CLOSE_MIN(5min)，实际 %v want %v", got, want)
	}
}

// TestPersonalOrderCloseAt_PoolCloseAt 订单在池中：用池内设备负载驱动的 CloseAt（最精确，与关单扫描器一致）。
func TestPersonalOrderCloseAt_PoolCloseAt(t *testing.T) {
	p := pool.NewWaitPayPool(30)
	now := time.Now()
	o := &model.Order{OutTradeNo: "POOL1", AccountID: 7, CreatedAt: now, Channel: "personal.alipay"}
	p.Add(o)
	app := &App{cfg: &Config{OrderCloseMinutes: 30, OrderCloseMin: 5, OrderCloseMax: 10}, waitPayPool: p}
	got := app.personalOrderCloseAt(o)
	if got.IsZero() {
		t.Fatalf("池内订单应返回 CloseAt，实际零值")
	}
	if !got.After(now) {
		t.Fatalf("池内 CloseAt 应在未来，实际 %v (now %v)", got, now)
	}
}

// TestPersonalOrderCloseAt_Disabled 自动关单未开启（所有窗口为 0）：返回零值（页面不显示倒计时）。
func TestPersonalOrderCloseAt_Disabled(t *testing.T) {
	app := &App{cfg: &Config{OrderCloseMinutes: 0, OrderCloseMin: 0, OrderCloseMax: 0}}
	o := &model.Order{OutTradeNo: "X", CreatedAt: time.Now(), Channel: "personal.alipay"}
	if got := app.personalOrderCloseAt(o); !got.IsZero() {
		t.Fatalf("自动关单未开启应返回零值，实际 %v", got)
	}
}
