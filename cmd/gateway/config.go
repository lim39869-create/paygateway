// paygateway 支付网关 config（从 main.go 拆分，配置结构/加载/env 解析/密钥）
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"paygateway/internal/channel"
	"paygateway/internal/logger"
	"paygateway/internal/store"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr string
	AdminAddr  string
	DBFile     string
	LogFile    string
	LogLevel   string

	JWTSecret string // JWT 签名密钥（留空自动生成）

	OrderCloseMinutes  int // 未支付订单自动关闭时间（分钟），0=关闭功能
	OrderCloseInterval int // 扫描间隔（秒）
	// 动态关单窗口上下限（分钟，PLAN-DYNAMIC-CLOSE-ALLOC §4.1）：
	// 窗口 = clamp(设备一轮耗时×1.5+支付操作预留, MIN, MAX)。0 或 MIN>MAX → 用静态 OrderCloseMinutes。
	OrderCloseMin int
	OrderCloseMax int
	// 设备处理能力：单设备可承载的 WAIT_PAY 订单上限（0=按公式自动算）。
	// 超限 → 分配器跳过该设备；全部超限 → 拒绝下单 + 告警建议加设备。
	DeviceLoadCap int
	// 设备降权阈值（占 DeviceLoadCap 比例，默认 0.8）：超过后该设备在分配器中降权。
	DeviceOverloadRatio float64
	// 单次 poll 成本估算（ms，动态窗口/设备能力计算；0=默认 4000，埋点校准后设置）
	DevicePollCostMS int

	// EpayPublicBase 对外易支付服务基础地址（其他项目对接用）
	// 同机项目填 http://127.0.0.1:8080；局域网填 http://<IP>:8080；公网填隧道/域名
	// 留空则自动推断为 http://127.0.0.1:<端口>
	EpayPublicBase string

	// 通道回调地址（启动时由隧道动态更新；账户密钥一律在管理后台「账户池」配置，不走环境变量）
	AlipayNotifyURL string
	WechatNotifyURL string

	// ChannelDebug 支付通道 SDK debug 日志（每笔请求/响应/签名全量 dump）
	// 默认关闭：真机事故——沙箱查单失败风暴时 gopay 每次失败刷 4 条 DEBUG，两天刷出 12w 行
	ChannelDebug bool

	// NotifyAllowPrivateIPs 允许商户 notify_url 指向私网/环回地址（SSRF 防护开关）。
	// 同机部署 New API 需要置 true（默认 true 保持兼容）；网关公网暴露且商户不可信时必须置 false。
	NotifyAllowPrivateIPs bool

	LedgerPrivKey    string // 审计密钥（空则自动生成并持久化到 settings，重启复用）
	AccountMasterKey string // 账户加密主密钥（空则自动生成）
	QrTokenSecret    string // 套码 token 签名密钥（空则自动生成并持久化）
	PersonalBaseURL  string // 个人免签公网基础地址（优先隧道域名，空则用 EpayPublicBase）

	// 个人免签风控（docs/MIANQIAN.md §10）：金额单位均为分
	PersonalMaxAmount    int64 // 单笔限额（默认 50000 = 500 元，0=不限）
	PersonalBigAmount    int64 // 大额告警阈值（默认 20000 = 200 元）
	MaxOrdersPerIPPerMin int   // 每 IP 每分钟下单上限（默认 10，0=不限；防脚本刷单）

	// 告警（RESILIENCE P1-3）
	AlertBarkURL    string // Bark 推送 URL（https://api.day.app/<key>），空=禁用
	AlertWebhookURL string // 通用 Webhook URL（POST JSON），空=禁用

	// 公网接口 IP 限流（防刷/防 DoS，仅作用于公网支付路由，不影响 /alipay/notify /wechat/notify 等签名回调）
	PublicRateMax       int
	PublicRateWindowSec int

	// 账本页服务端监听（docs/PLAN-ADB-LEDGER-IMPL.md §6.3）
	LedgerPollInterval   time.Duration // 无活跃订单轮询间隔（默认 5s）
	LedgerActiveInterval time.Duration // 有活跃订单轮询间隔（默认 1s）

	// 通知栏快速通道（NotifyWatcher，2026-08-30 新增）
	NotifyPollInterval time.Duration // 有 WAIT_PAY 订单轮询间隔（默认 500ms）
	NotifyIdleInterval time.Duration // 池空低频探测间隔（默认 5s）

	// TunnelHostname 固定隧道域名（如 paygateway.de5.net）。空 = 快速隧道随机域名。
	// 命名隧道需要 ~/.cloudflared/paygateway.yml + paygateway.json 凭据。
	TunnelHostname string
}

// loadEnv 加载 .env 文件到环境变量
func loadEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // 文件不存在就跳过
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if key != "" && val != "" {
			os.Setenv(key, val)
		}
	}
}

func loadConfig() *Config {
	return &Config{
		ListenAddr: envOr("GATEWAY_ADDR", ":8080"),
		AdminAddr:  envOr("ADMIN_ADDR", ":8081"),
		DBFile:     envOr("ORDER_DB", "data/paygateway.db"),
		LogFile:    envOr("LOG_FILE", "logs/paygateway-{date}.log"),
		LogLevel:   envOr("LOG_LEVEL", "info"),
		JWTSecret:  envOr("JWT_SECRET", ""),

		OrderCloseMinutes:   envInt("ORDER_CLOSE_MINUTES", 30),
		OrderCloseInterval:  envInt("ORDER_CLOSE_INTERVAL", 30),
		OrderCloseMin:       envInt("ORDER_CLOSE_MIN", 5),
		OrderCloseMax:       envInt("ORDER_CLOSE_MAX", 10),
		DeviceLoadCap:       envInt("DEVICE_LOAD_CAP", 0),
		DeviceOverloadRatio: envFloat("DEVICE_OVERLOAD_RATIO", 0.8),
		DevicePollCostMS:    envInt("DEVICE_POLL_COST_MS", 4000),
		EpayPublicBase:      envOr("EPAY_PUBLIC_BASE_URL", ""),
		AlipayNotifyURL:     envOr("ALIPAY_NOTIFY_URL", "http://localhost:8080/alipay/notify"),
		WechatNotifyURL:     envOr("WECHAT_NOTIFY_URL", "http://localhost:8080/wechat/notify"),
		ChannelDebug:        envOr("CHANNEL_DEBUG", "0") == "1",

		NotifyAllowPrivateIPs: envBool("NOTIFY_ALLOW_PRIVATE_IPS", true),
		LedgerPrivKey:         envOr("LEDGER_PRIVATE_KEY", ""),
		AccountMasterKey:      envOr("ACCOUNT_MASTER_KEY", ""),
		QrTokenSecret:         envOr("QR_TOKEN_SECRET", ""),
		PersonalBaseURL:       envOr("PERSONAL_BASE_URL", ""),
		PersonalMaxAmount:     envInt64("PERSONAL_MAX_AMOUNT", 50000),
		PersonalBigAmount:     envInt64("PERSONAL_BIG_AMOUNT", 20000),
		MaxOrdersPerIPPerMin:  envInt("MAX_ORDERS_PER_IP_PER_MIN", 10),
		AlertBarkURL:          envOr("ALERT_BARK_URL", ""),
		AlertWebhookURL:       envOr("ALERT_WEBHOOK_URL", ""),
		PublicRateMax:         envInt("PUBLIC_RATE_MAX", 60),
		PublicRateWindowSec:   envInt("PUBLIC_RATE_WINDOW_SEC", 60),
		LedgerPollInterval:    envDur("LEDGER_POLL_INTERVAL", 5*time.Second),
		LedgerActiveInterval:  envDur("LEDGER_ACTIVE_INTERVAL", 1*time.Second),
		NotifyPollInterval:    envDur("NOTIFY_POLL_INTERVAL", 500*time.Millisecond),
		NotifyIdleInterval:    envDur("NOTIFY_IDLE_INTERVAL", 5*time.Second),
		TunnelHostname:        envOr("CF_TUNNEL_HOSTNAME", ""),
	}
}

func envInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return def
}

// applySettings 从 DB 加载运行时设置，覆盖 .env 配置
func applySettings(st store.Store, cfg *Config) {
	ctx := context.Background()
	settings, err := st.GetAllSettings(ctx)
	if err != nil || len(settings) == 0 {
		return
	}
	if v, ok := settings["epay_public_base_url"]; ok && v != "" {
		cfg.EpayPublicBase = v
	}
	if v, ok := settings["order_close_minutes"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.OrderCloseMinutes = n
		}
	}
	logger.Info("main", "运行时设置已加载", "count", len(settings))
}

// resolveLedgerKey 解析审计账本私钥：.env > settings 表 > 首次生成并持久化到 settings
// 保证重启后密钥不变，旧账本条目验签可继续通过
func resolveLedgerKey(st store.Store, cfg *Config) (string, error) {
	ctx := context.Background()
	// 1. .env 优先
	if cfg.LedgerPrivKey != "" {
		return cfg.LedgerPrivKey, nil
	}
	// 2. settings 表
	settings, err := st.GetAllSettings(ctx)
	if err == nil {
		if k, ok := settings["ledger_private_key"]; ok && k != "" {
			return k, nil
		}
	}
	// 3. 生成并持久化
	key, err := generateLedgerKeyPEM()
	if err != nil {
		return "", err
	}
	if err := st.SetSetting(ctx, "ledger_private_key", key); err != nil {
		return "", fmt.Errorf("保存审计密钥到 settings 失败: %w", err)
	}
	logger.Info("main", "已生成新的审计账本密钥并持久化到 settings（重启复用）")
	return key, nil
}

// generateLedgerKeyPEM 生成 ECDSA P-256 私钥 PEM
func generateLedgerKeyPEM() (string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", fmt.Errorf("生成审计密钥失败: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("序列化审计密钥失败: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

// applyChannelRuntimeSettings 从 DB 加载通道运行时设置（需在通道注册后调用）
// 包括：通道启停状态 + 支付模式启停
func applyChannelRuntimeSettings(st store.Store) {
	ctx := context.Background()
	settings, err := st.GetAllSettings(ctx)
	if err != nil || len(settings) == 0 {
		return
	}
	// 通道启用状态
	for _, name := range channel.AllNames() {
		if v, ok := settings["channel_enabled_"+name]; ok {
			channel.SetEnabled(name, v == "true")
		}
	}
	// 支付宝支付模式启停
	if ch, ok := channel.Lookup("merchant.alipay"); ok {
		type modeSetter interface {
			SetModeEnabled(mode string, enabled bool)
		}
		if ms, ok := ch.(modeSetter); ok {
			for _, mode := range []string{"page", "wap", "qrcode"} {
				if v, ok := settings["alipay_mode_"+mode]; ok {
					ms.SetModeEnabled(mode, v == "true")
				}
			}
		}
	}
	logger.Info("main", "通道运行时设置已加载", "count", len(settings))
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

// ========== 依赖容器 ==========
