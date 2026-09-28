package personal

import (
	"testing"
	"time"
)

// TestRateLimiterAllow 基础限流：超限拒绝
func TestRateLimiterAllow(t *testing.T) {
	rl := NewRateLimiter(3, time.Minute, 0)
	if !rl.Allow("dev-1") {
		t.Fatal("第1次应允许")
	}
	if !rl.Allow("dev-1") {
		t.Fatal("第2次应允许")
	}
	if !rl.Allow("dev-1") {
		t.Fatal("第3次应允许")
	}
	if rl.Allow("dev-1") {
		t.Fatal("第4次应拒绝（窗口内已满）")
	}
	if rl.Count("dev-1") != 3 {
		t.Fatalf("窗口内次数应为 3，实际 %d", rl.Count("dev-1"))
	}
}

// TestRateLimiterWindow 窗口滑动：旧记录过期后恢复
func TestRateLimiterWindow(t *testing.T) {
	rl := NewRateLimiter(2, time.Second, 0) // 1 秒窗口
	if !rl.Allow("dev-2") || !rl.Allow("dev-2") {
		t.Fatal("前两次应允许")
	}
	if rl.Allow("dev-2") {
		t.Fatal("第3次应拒绝")
	}
	time.Sleep(1100 * time.Millisecond) // 窗口过期
	if !rl.Allow("dev-2") {
		t.Fatal("窗口过期后应恢复允许")
	}
}

// TestRateLimiterPerKey 不同 key 相互独立
func TestRateLimiterPerKey(t *testing.T) {
	rl := NewRateLimiter(1, time.Minute, 0)
	if !rl.Allow("dev-a") {
		t.Fatal("dev-a 第1次应允许")
	}
	if rl.Allow("dev-a") {
		t.Fatal("dev-a 第2次应拒绝")
	}
	if !rl.Allow("dev-b") {
		t.Fatal("dev-b 应独立允许")
	}
}

// TestRateLimiterNoLimit 未配置限额时全部放行
func TestRateLimiterNoLimit(t *testing.T) {
	rl := NewRateLimiter(0, time.Minute, 0)
	for i := 0; i < 100; i++ {
		if !rl.Allow("dev-x") {
			t.Fatal("无限额时不应拒绝")
		}
	}
}
