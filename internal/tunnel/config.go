package tunnel

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config 隧道配置
type Config struct {
	LocalPort    int              // 本地服务端口（默认 8080）
	CallbackPath string           // 回调路径（如 /alipay/notify）
	StartTimeout time.Duration    // 等待隧道就绪超时（默认 30s）
	Connections  int              // cloudflared 连接数（默认 4，高并发可调大）
	OnURLChange  func(url string) // URL 变化回调（隧道重启换域名时自动调用）
	// Hostname 固定域名（如 paygateway.de5.net）。
	// 非空 = 命名隧道模式（cloudflared tunnel run，需 ~/.cloudflared/paygateway.yml + paygateway.json 凭据）；
	// 空 = 默认快速隧道（trycloudflare.com 随机域名）。
	Hostname string
	// ConfigPath 命名隧道的 cloudflared 配置文件路径（默认 ~/.cloudflared/paygateway.yml）��
	// 仅 Hostname 非空时生效。
	ConfigPath string
}

// DefaultConfig 默认配置
func DefaultConfig() *Config {
	return &Config{
		LocalPort:    8080,
		CallbackPath: "/alipay/notify",
		StartTimeout: 30 * time.Second,
		Connections:  4,
	}
}

// SetDefaultConfigPath 返回默认的 cloudflared 配置文件路径（~/.cloudflared/paygateway.yml）。
// 外部（main.go）可调用本函数设置 Config.ConfigPath；留空则 tunnel.New 使用此默认路径。
func SetDefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "paygateway.yml"
	}
	return filepath.Join(home, ".cloudflared", "paygateway.yml")
}

// EnsureCredentials 从环境变量生成 cloudflared 凭据和配置文件。
//
// 环境变量：
//   - CF_TUNNEL_HOSTNAME   — 固定域名（如 paygateway.de5.net），空 = 跳过
//   - CF_TUNNEL_CREDENTIALS — tunnel 凭据 JSON 的 base64（由 setup-domain.sh 或首次启动写入 .env）
//
// 写入文件：
//
//	~/.cloudflared/paygateway.json （凭据）
//	~/.cloudflared/paygateway.yml  （隧道配置）
//
// 幂等：文件已存在则跳过写入，不影响运行中隧道。换环境时只需复制 .env 即可。
func EnsureCredentials() error {
	hostname := os.Getenv("CF_TUNNEL_HOSTNAME")
	b64 := os.Getenv("CF_TUNNEL_CREDENTIALS")
	if hostname == "" || b64 == "" {
		return nil // 快速隧道模式或未配置，跳过
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return fmt.Errorf("CF_TUNNEL_CREDENTIALS base64 解码失败: %w", err)
	}
	// 解析凭据提取 TunnelID
	var cred struct {
		TunnelID string `json:"TunnelID"`
	}
	if err := json.Unmarshal(raw, &cred); err != nil {
		return fmt.Errorf("CF_TUNNEL_CREDENTIALS JSON 解析失败: %w", err)
	}
	if cred.TunnelID == "" {
		return fmt.Errorf("CF_TUNNEL_CREDENTIALS 缺少 TunnelID")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("获取 HOME 目录失败: %w", err)
	}
	cfgDir := filepath.Join(home, ".cloudflared")
	credFile := filepath.Join(cfgDir, "paygateway.json")
	ymlFile := filepath.Join(cfgDir, "paygateway.yml")

	// 创建目录
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", cfgDir, err)
	}
	// 写凭据文件（幂等）
	if _, err := os.Stat(credFile); os.IsNotExist(err) {
		if err := os.WriteFile(credFile, raw, 0600); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", credFile, err)
		}
	}
	// 写配置文件（幂等）
	if _, err := os.Stat(ymlFile); os.IsNotExist(err) {
		yml := fmt.Sprintf(`tunnel: %s
credentials-file: %s

ingress:
  - hostname: %s
    service: http://localhost:8080
  - service: http_status:404
`, cred.TunnelID, credFile, hostname)
		if err := os.WriteFile(ymlFile, []byte(yml), 0600); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", ymlFile, err)
		}
	}
	return nil
}
