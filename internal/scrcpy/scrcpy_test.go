package scrcpy

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestLiveSession 集成测试：需要一台 adb 已连接的真机/设备
// 运行条件：环境变量 PG_TEST_SERIAL 指向一个 adb serial（USB 直连如 "4f2bdc30" 或 adb over WiFi 如 "127.0.0.1:5556"），设备在线
// 手动运行：PG_TEST_SERIAL=4f2bdc30 go test ./internal/scrcpy/ -run TestLiveSession -v
func TestLiveSession(t *testing.T) {
	serial := os.Getenv("PG_TEST_SERIAL")
	if serial == "" {
		t.Skip("未设置 PG_TEST_SERIAL（如 PG_TEST_SERIAL=127.0.0.1:5556）")
	}
	ctx := context.Background()
	sess, err := StartSession(ctx, serial, 27184)
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	defer sess.Close()

	conn := sess.Connection()
	t.Logf("设备: %s, 分辨率: %dx%d", conn.Device, conn.Width, conn.Height)
	if conn.Width == 0 {
		// 等待 session meta
		meta, err := conn.ReadSessionMeta()
		if err != nil {
			t.Fatalf("ReadSessionMeta: %v", err)
		}
		t.Logf("session meta: %dx%d", meta.Width, meta.Height)
	}

	// 读几帧验证视频流
	conn.video.SetReadDeadline(time.Now().Add(5 * time.Second))
	frames := 0
	for i := 0; i < 5; i++ {
		f, err := conn.ReadVideo()
		if err != nil {
			t.Fatalf("ReadVideo(%d): %v", i, err)
		}
		frames++
		t.Logf("帧%d: %d 字节, config=%v keyframe=%v", i+1, len(f.Data), f.Config, f.KeyFrame)
		if i == 0 && !f.Config {
			t.Log("⚠️ 首帧不是 config（SPS/PPS），浏览器可能无法解码")
		}
	}

	// 注入触摸
	if err := conn.InjectTouch(0, 100, 100); err != nil { // DOWN
		t.Fatalf("InjectTouch DOWN: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := conn.InjectTouch(1, 100, 100); err != nil { // UP
		t.Fatalf("InjectTouch UP: %v", err)
	}
	t.Logf("✅ 触摸注入成功，共收到 %d 帧", frames)
	_ = fmt.Sprint()
}
