// Package tunnel Cloudflare Tunnel 管理：启动隧道、获取回调 URL、健康检查
//
// 使用 trycloudflare.com 快速隧道（无需认证），每次启动获取随机域名。
// 启动后通过 GetCallbackURL() 获取当前可用回调地址。
package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"paygateway/internal/logger"
)

// Tunnel 单个隧道实例
type Tunnel struct {
	url         string // https://xxx.trycloudflare.com 或 https://<hostname>
	hostname    string // 命名隧道的原始 hostname（paygateway.de5.net）；快速隧道为空
	localPort   int
	connections int // ha-connections 数（重启时保留，避免传 0 给 cloudflared）
	cmd         *exec.Cmd
	cancel      context.CancelFunc
	healthy     bool
	done        chan struct{}
	mu          sync.RWMutex
	onURLChange func(url string) // URL 变化回调
}

// New 创建并启动隧道
// 阻塞直到隧道就绪或超时
func New(ctx context.Context, cfg *Config) (*Tunnel, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}

	tunCtx, cancel := context.WithCancel(ctx)

	// 使用编译时嵌入的 cloudflared 二进制
	cfPath, err := cloudflaredPath()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("获取 cloudflared 路径失败: %w", err)
	}

	var cmd *exec.Cmd
	if cfg.Hostname != "" {
		// 命名隧道模式：cloudflared tunnel run --config <path>
		configPath := cfg.ConfigPath
		if configPath == "" {
			configPath = defaultConfigPath()
		}
		if _, err := os.Stat(configPath); err != nil {
			cancel()
			return nil, fmt.Errorf("命名隧道配置文件不存在: %s（请先创建 %s，含 ingress 路由）", configPath, configPath)
		}
		cmd = exec.CommandContext(tunCtx, cfPath,
			"tunnel", "--no-autoupdate",
			"--config", configPath,
			"run",
		)
	} else {
		// 快速隧道模式：trycloudflare 随机域名
		cmd = exec.CommandContext(tunCtx, cfPath,
			"tunnel", "--no-autoupdate",
			"--url", fmt.Sprintf("http://localhost:%d", cfg.LocalPort),
			"--ha-connections", fmt.Sprintf("%d", cfg.Connections),
		)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("创建 stderr pipe 失败: %w", err)
	}

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("启动 cloudflared 失败: %w", err)
	}

	t := &Tunnel{
		localPort:   cfg.LocalPort,
		connections: cfg.Connections,
		hostname:    cfg.Hostname,
		cmd:         cmd,
		cancel:      cancel,
		done:        make(chan struct{}),
		onURLChange: cfg.OnURLChange,
	}

	if cfg.Hostname != "" {
		// 命名隧道：URL 固定为 https://<hostname>，就绪判定 = cloudflared 注册成功
		t.url = "https://" + cfg.Hostname
		t.healthy = true
		// 异步等待连接注册成功（读到 Registered tunnel connection 即视为就绪）
		go t.waitNamedReady(stderr)
		logger.Info("tunnel", "命名隧道已启动（等待连接注册）", "url", t.url, "config", cfg.ConfigPath)
		if cfg.OnURLChange != nil {
			cfg.OnURLChange(t.url)
		}
		return t, nil
	}

	// 异步读取 stderr 获取域名
	domainCh := make(chan string, 1)
	go t.readDomain(stderr, domainCh)

	// 等待域名就绪
	select {
	case url := <-domainCh:
		t.mu.Lock()
		t.url = url
		t.healthy = true
		t.mu.Unlock()
		logger.Info("tunnel", "隧道就绪", "url", url)
		// 通知 URL 变化
		if cfg.OnURLChange != nil {
			cfg.OnURLChange(url)
		}
		return t, nil

	case <-time.After(cfg.StartTimeout):
		cancel()
		cmd.Wait()
		return nil, fmt.Errorf("隧道启动超时（%v）", cfg.StartTimeout)

	case <-ctx.Done():
		cancel()
		cmd.Wait()
		return nil, ctx.Err()
	}
}

// waitNamedReady 等待命名隧道连接注册成功；注册失败/进程退出时标记不健康。
// 注册成功后继续排空 stderr：cloudflared 持续写日志，若停止读取 pipe 缓冲区写满会阻塞
// cloudflared 主循环，导致隧道假死（健康检查失败 → 无限重启）。
func (t *Tunnel) waitNamedReady(stderr io.ReadCloser) {
	defer close(t.done)
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 1024*64), 1024*64)
	registered := false
	for scanner.Scan() {
		line := scanner.Text()
		if !registered {
			if strings.Contains(line, "Registered tunnel connection") {
				logger.Info("tunnel", "命名隧道连接已注册", "url", t.URL())
				registered = true
			}
			if strings.Contains(line, "no more connections active and exiting") ||
				strings.Contains(line, "Fatal") {
				t.mu.Lock()
				t.healthy = false
				t.mu.Unlock()
				logger.Warn("tunnel", "命名隧道连接异常退出", "line", line)
				return
			}
		}
	}
}

// readDomain 从 cloudflared stderr 中解析 trycloudflare 隧道域名
// 注意：cloudflared 早期日志会输出 api.trycloudflare.com（官方 API 地址），
// 它不是隧道域名，必须跳过，否则 notify_url 会变成 https://api.trycloudflare.com/... 导致回调 404
func (t *Tunnel) readDomain(stderr io.ReadCloser, domainCh chan<- string) {
	defer close(t.done)
	re := regexp.MustCompile(`https://([a-z0-9-]+\.trycloudflare\.com)`)
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 1024*64), 1024*64)
	for scanner.Scan() {
		line := scanner.Text()
		if matches := re.FindStringSubmatch(line); len(matches) >= 2 {
			host := matches[1]
			// 排除 Cloudflare 官方控制面域名（非隧道地址）
			if host == "api.trycloudflare.com" || host == "dash.trycloudflare.com" {
				continue
			}
			select {
			case domainCh <- "https://" + host:
			default:
			}
			return
		}
	}
}

// URL 返回隧道公网地址（如 https://xxx.trycloudflare.com）
func (t *Tunnel) URL() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.url
}

// CallbackURL 返回完整的回调地址
func (t *Tunnel) CallbackURL(callbackPath string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.url == "" {
		return ""
	}
	return t.url + callbackPath
}

// Healthy 返回隧道是否健康
func (t *Tunnel) Healthy() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.healthy
}

// Stop 停止隧道
func (t *Tunnel) Stop() {
	if t.cancel != nil {
		t.cancel()
	}
	_ = t.cmd.Wait()
}

// HealthLoop 定期健康检查，自动重启
func (t *Tunnel) HealthLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.checkHealth(ctx)
		}
	}
}

// checkHealth 检查隧道健康。
// 注意：不走公网 URL 自环（网络封锁/ISP 劫持会使公网 /health 永远不可达 → 误判不健康 → 无限重启风暴）。
// 改为：本地 8080 可达 + cloudflared 进程存活 = 健康（进程死了才重启，URL 变才触发回调）。
func (t *Tunnel) checkHealth(ctx context.Context) {
	t.mu.Lock()
	wasHealthy := t.healthy
	url := t.url
	t.mu.Unlock()
	if url == "" {
		return
	}

	// 1. 本地源站可达（不经隧道，绕开公网链路）
	localOK := t.checkLocal(ctx)
	// 2. cloudflared 进程存活
	procOK := t.cmd != nil && t.cmd.Process != nil && t.cmd.ProcessState == nil
	healthy := localOK && procOK

	t.mu.Lock()
	t.healthy = healthy
	t.mu.Unlock()

	if wasHealthy && !healthy {
		logger.Warn("tunnel", "隧道不健康，准备重启", "url", url, "local_ok", localOK, "proc_ok", procOK)
		go t.restart(ctx)
	} else if !wasHealthy && healthy {
		logger.Info("tunnel", "隧道恢复健康", "url", url)
	}
}

// checkLocal 检测本地源站是否可达（http://localhost:<port>/health）
func (t *Tunnel) checkLocal(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, "GET",
		fmt.Sprintf("http://localhost:%d/health", t.localPort), nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (t *Tunnel) restart(ctx context.Context) {
	// 已不再持有旧进程，立即标记为不健康
	t.mu.Lock()
	t.healthy = false
	t.mu.Unlock()
	// 停止旧进程（调用 cancel 后 cmd 会退出，Wait 清理僵尸）
	if t.cancel != nil {
		t.cancel()
		_ = t.cmd.Wait()
	}
	time.Sleep(2 * time.Second)

	// 退避重试：失败后递增等待（30s/60s/120s/240s… 最高 300s），直到成功或主 ctx 取消
	backoff := 30 * time.Second
	maxBackoff := 300 * time.Second
	for {
		newT, err := New(ctx, &Config{
			LocalPort:    t.localPort,
			StartTimeout: 30 * time.Second,
			Connections:  t.connections,
			OnURLChange:  t.onURLChange,
			Hostname:     t.hostname,
		})
		if err == nil {
			// 替换自身
			t.mu.Lock()
			oldURL := t.url
			t.url = newT.url
			t.healthy = newT.healthy
			t.cmd = newT.cmd
			t.cancel = newT.cancel
			t.done = newT.done
			t.mu.Unlock()
			logger.Info("tunnel", "隧道重启成功", "url", newT.url)
			// URL 变了，通知回调
			if newT.url != oldURL && newT.url != "" && t.onURLChange != nil {
				t.onURLChange(newT.url)
			}
			return
		}
		logger.Error("tunnel", "隧道重启失败，即将重试", "err", err, "backoff_sec", int(backoff.Seconds()))
		select {
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		case <-ctx.Done():
			logger.Warn("tunnel", "隧道退出重试（主 ctx 已取消）")
			return
		}
	}
}

// defaultConfigPath 返回命名隧道默认配置文件路径（与 gateway 同用户 ~/.cloudflared/paygateway.yml）
func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "paygateway.yml"
	}
	return filepath.Join(home, ".cloudflared", "paygateway.yml")
}
