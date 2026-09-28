// Session 会话管理：通过 adb 启动 scrcpy-server 并建立连接
//
// 流程（对应 doc/develop.md）：
//  1. push scrcpy-server.jar 到设备 /data/local/tmp/
//  2. adb forward tcp:<port> localabstract:scrcpy
//  3. app_process 启动 Server（scid=-1 → socket 名 "scrcpy"）
//  4. Dial 连接（video → control → metadata）
package scrcpy

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"paygateway/internal/logger"
)

// ServerJar 嵌入的 scrcpy-server.jar（编译时由脚本下载，go:generate）
//
//go:generate bash ../../scripts/download-scrcpy-server.sh
//go:embed scrcpy-server.jar
var ServerJar []byte

// Session 单个设备的 scrcpy 会话
type Session struct {
	mu     sync.Mutex
	serial string // adb serial（host:port）
	port   int    // 本机 forward 端口
	conn   *Connection
	cancel context.CancelFunc
	done   chan struct{}
}

// StartSession 启动并连接设备。
// 关键设计点：
//   - 总超时 30s、socketReady 8s（USB 真机起 server 一般 <5s）
//   - socketReady 用 /proc/net/unix 字节扫描，不依赖 shell grep（精简 ROM 无该命令）
//   - 不重试 Dial：每次 Dial 消耗 server 一次 accept，重试会破坏协议状态机
//   - 任何失败必先 cleanupAdb：清 forward + pkill（避免端口耗光与旧 server 抢 accept）
func StartSession(parent context.Context, serial string, port int) (*Session, error) {
	s := &Session{serial: serial, port: port, done: make(chan struct{})}
	s.cancel = func() {}

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	t0 := time.Now()
	if err := s.ensureServer(ctx); err != nil {
		s.cleanupAdb()
		return nil, err
	}
	tEnsure := time.Since(t0)
	if !s.waitSocketReady(ctx, 8*time.Second) {
		s.cleanupAdb()
		return nil, fmt.Errorf("scrcpy server 未在 8s 监听 socket（设备可能锁屏/无 root 权限/app_process 不可用）")
	}
	tSocket := time.Since(t0)
	time.Sleep(100 * time.Millisecond) // dial 前 dwell：让 server 进入 accept 队列
	conn, err := Dial(fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		s.cleanupAdb()
		return nil, fmt.Errorf("连接 scrcpy: %w", err)
	}
	logger.Info("scrcpy", "会话建立", "serial", s.serial, "port", s.port,
		"ensure_ms", tEnsure.Milliseconds(), "socket_ms", tSocket.Milliseconds(), "dial_ms", time.Since(t0).Milliseconds())
	s.conn = conn
	return s, nil
}

// socketReady 通过 /proc/net/unix 检查 scrcpy abstract socket 是否监听
// 不走 `grep scrcpy` 是因为精简 ROM 可能没有 grep；改为整文件读 + Go 字节扫描
func (s *Session) socketReady() bool {
	out, err := s.adb("shell", "cat /proc/net/unix")
	if err != nil {
		return false
	}
	return strings.Contains(out, "scrcpy")
}

func (s *Session) waitSocketReady(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		default:
		}
		if s.socketReady() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func (s *Session) cleanupAdb() {
	_, _ = s.adb("forward", "--remove", fmt.Sprintf("tcp:%d", s.port))
	_ = s.adbShell("pkill -f 'com.genymobile.scrcpy.Server' || true")
}

// ensureServer 确保 jar 已推送 + server 进程运行 + forward 建立
// ctx 用于约束每条 adb 子命令的超时：USB 拔了 adb 默认会卡死，必须超时
func (s *Session) ensureServer(ctx context.Context) error {
	out, _ := s.adbCtx(ctx, "shell", "test -f /data/local/tmp/scrcpy-server.jar && echo OK || echo MISSING")
	if strings.Contains(out, "MISSING") {
		if err := s.adbPushJar(ctx); err != nil {
			return fmt.Errorf("推送 scrcpy-server: %w", err)
		}
	}

	_, _ = s.adbCtx(ctx, "forward", "--remove", fmt.Sprintf("tcp:%d", s.port))
	_, _ = s.adbCtx(ctx, "shell", "pkill -f 'com.genymobile.scrcpy.Server' || true")

	if _, err := s.adbCtx(ctx, "forward", fmt.Sprintf("tcp:%d", s.port), "localabstract:scrcpy"); err != nil {
		return fmt.Errorf("adb forward: %w", err)
	}

	// stay_awake=true：scrcpy 持有 WakeLock 保持设备屏幕唤醒。
	// 不加的话设备熄屏 → Android 不再向 MediaCodec 提供 Surface 帧 → 视频流静默停止，
	// 而 control socket 仍通（鼠标注入"看似有效"但画面黑）——即控制台再次打开黑屏 bug。
	startCmd := "nohup sh -c 'CLASSPATH=/data/local/tmp/scrcpy-server.jar app_process / com.genymobile.scrcpy.Server 4.1 " +
		"scid=-1 video=true audio=false control=true tunnel_forward=true cleanup=false stay_awake=true max_size=1080' > /dev/null 2>&1 &"
	if _, err := s.adbCtx(ctx, "shell", startCmd); err != nil {
		return fmt.Errorf("启动 scrcpy-server: %w", err)
	}
	return nil
}

// adbPushJar 推送 jar 到设备
// 每次 5s 超时；首次失败重试一次（USB 真机偶发 adb busy，重试一次基本必过）
func (s *Session) adbPushJar(ctx context.Context) error {
	if len(ServerJar) == 0 {
		return fmt.Errorf("scrcpy-server.jar 未嵌入（请先运行 go generate）")
	}
	tmp, err := os.CreateTemp("", "scrcpy-server-*.jar")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(ServerJar); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	doPush := func() error {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cctx, "adb", "-s", s.serial, "push", tmpPath, "/data/local/tmp/scrcpy-server.jar")
		var errb bytes.Buffer
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("adb push: %w (%s)", err, strings.TrimSpace(errb.String()))
		}
		return nil
	}
	if err := doPush(); err != nil {
		time.Sleep(200 * time.Millisecond)
		return doPush()
	}
	return nil
}

// adb 带固定 8s 超时执行；USB 拔了 adb 默认无超时会卡死整个 session
func (s *Session) adb(args ...string) (string, error) {
	return s.adbCtx(context.Background(), args...)
}

func (s *Session) adbCtx(ctx context.Context, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	full := append([]string{"-s", s.serial}, args...)
	cmd := exec.CommandContext(cctx, "adb", full...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("adb %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Connection 返回活动连接
func (s *Session) Connection() *Connection { return s.conn }

func (s *Session) adbShell(cmd string) error {
	_, err := s.adb("shell", cmd)
	return err
}

// Close 关闭会话（断开连接 + 停止 server）
// 幂等：deviceSession.videoLoop 退出时也可能间接调用，避免 close(done) 双触发 panic
func (s *Session) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
		s.conn = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.adb("forward", "--remove", fmt.Sprintf("tcp:%d", s.port))
	s.adbShell("pkill -f 'com.genymobile.scrcpy.Server' || true")
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}
