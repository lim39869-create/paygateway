// Package personal 个人免签通道核心：备注码分配、套码防劫持、匹配引擎
//
// 设计文档：docs/MIANQIAN.md
// 核心思路：个人收款码 + 系统生成备注码 + 账本读取（adb）→ 服务端精确匹配入账。
// 所有资金写入只走 order.Service 幂等入口（SettlePayment），本包不做任何直接状态修改。
package personal

import (
	"encoding/json"
	"time"
)

// DeviceStatus 设备状态（由 DeviceWatcher 按 adb 可达性维护）
type DeviceStatus string

const (
	DeviceOnline  DeviceStatus = "online"  // 在线（adb 可达）
	DeviceOffline DeviceStatus = "offline" // 离线（adb 不可达）
	DeviceFrozen  DeviceStatus = "frozen"  // 手动冻结（不可接新单）
)

// ControlState 控制面状态（服务端 adb 通路的可达性）
//   - usb         : USB 直连或 adb over WiFi 正常（control_state=usb）
//   - tunnel_only : 仅报文面可达，adb 不通
//
// 2026-08-29 重构：监听 App 弃用，relay（CF wss 反代隧道）状态已移除。
const (
	ControlStateUSB        = "usb"
	ControlStateTunnelOnly = "tunnel_only"
)

// Device 真机设备（personal.db personal_devices 表）
// 每个设备绑定一个收款账户（account_id），承载账本读取
type Device struct {
	ID        int64        `json:"id"`
	DeviceID  string       `json:"device_id"` // 客户端 UUID（首次启动生成）
	Name      string       `json:"name"`      // 备注名（如"卧室安卓机"）
	AccountID int64        `json:"account_id"`
	Provider  string       `json:"provider"` // alipay / wechat
	Secret    string       `json:"-"`        // 通信密钥（仅内存，加密存储）
	Status    DeviceStatus `json:"status"`
	LastPush  time.Time    `json:"last_push"`
	IP        string       `json:"ip"`
	// adb 连接信息（web 可视化操作设备用）
	// AdbHost 为 USB 直连设备 serial（如 "4f2bdc30"，无端口）或 host（如 "127.0.0.1"）
	AdbHost string `json:"adb_host,omitempty"` // USB serial 或 host（首选通道）
	AdbPort int    `json:"adb_port,omitempty"` // 如 5555；0 = USB 直连（无端口）
	// WiFi 备用通道（USB 断线时自动切换；与 AdbHost 同网段）
	AdbWifiHost string `json:"adb_wifi_host,omitempty"` // 如 192.168.1.119
	AdbWifiPort int    `json:"adb_wifi_port,omitempty"` // 如 5555；0 = 未配置
	// 引导向导完成度（JSON 数组字符串，与 DeviceEncrypted.SetupProgress 同语义；"" 视为 "[]"）
	SetupProgress string `json:"setup_progress,omitempty"`
	// TsIP 设备组网 IP（Tailscale 100.x.x.x，未上线为空）。P2P 控制面恢复 adb 时使用。
	TsIP string `json:"ts_ip,omitempty"`
	// AdbListen adbd TCP 监听是否就绪（adb 探测，0/1）
	AdbListen bool `json:"adb_listen,omitempty"`
	// ControlState 控制面状态：usb / tunnel_only
	ControlState string    `json:"control_state,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// DeviceEncrypted personal.db 存储版本（secret 加密）
type DeviceEncrypted struct {
	ID          int64  `json:"id"`
	DeviceID    string `json:"device_id"`
	Name        string `json:"name"`
	AccountID   int64  `json:"account_id"`
	Provider    string `json:"provider"`
	SecretEnc   string `json:"secret_enc"`
	Status      string `json:"status"`
	LastPush    string `json:"last_push"`
	IP          string `json:"ip"`
	AdbHost     string `json:"adb_host,omitempty"`
	AdbPort     int    `json:"adb_port,omitempty"`
	AdbWifiHost string `json:"adb_wifi_host,omitempty"`
	AdbWifiPort int    `json:"adb_wifi_port,omitempty"`
	// SetupProgress 引导向导完成度（JSON 数组字符串，元素取自 SetupStep* 常量）。
	// 空数组或 "[]" 表示新建未配置；含全部 4 步表示接入完成可参与接单。
	SetupProgress string `json:"setup_progress,omitempty"`
	// TsIP 设备组网 IP（Tailscale 100.x.x.x，未上线为空）。P2P 控制面恢复 adb 时使用。
	TsIP string `json:"ts_ip,omitempty"`
	// AdbListen adbd TCP 监听是否就绪（adb 探测，0/1）
	AdbListen int `json:"adb_listen,omitempty"`
	// ControlState 控制面状态：usb / tunnel_only
	ControlState string `json:"control_state,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// 引导向导完成的阶段（SetupProgress 数组元素值）。改这些常量需同步改 wizard 接口与前端。
// 2026-08-30 重构：监听端 APP 弃用，向导收敛为 2 步（usb_connected → bound），
// 设备上线由 DeviceWatcher 按 adb 可达性驱动，不再有 apk_installed/config_pushed/heartbeat_connected。
const (
	SetupStepUSBConnected = "usb_connected" // 第 1 步：USB 真机连上，adb devices state=device
	SetupStepBound        = "bound"         // 第 2 步：已绑定收款账户（personal_devices 记录建好）
)

// Push 收款事件（personal.db personal_pushes 表，审计用）
type Push struct {
	ID           int64     `json:"id"`
	DeviceID     string    `json:"device_id"`
	Provider     string    `json:"provider"`
	AmountMinor  int64     `json:"amount_minor"`
	Note         string    `json:"note"` // 备注码（可空）
	RawHash      string    `json:"raw_hash"`
	MatchedOrder string    `json:"matched_order"`       // 匹配到的订单号（可空）
	MatchResult  string    `json:"match_result"`        // matched / no_order / ambiguous / amount_fallback
	RiskText     string    `json:"risk_text,omitempty"` // 风控文案（封号监控上报）
	Space        int       `json:"space"`               // 通知来源空间：0=主空间，999=双开(XSpace)
	Raw          string    `json:"raw"`                 // 通知原文
	CreatedAt    time.Time `json:"created_at"`
}

// UnmatchedPayment 无订单收款（personal.db unmatched_payments 表，人工处理）
type UnmatchedPayment struct {
	ID          int64  `json:"id"`
	Provider    string `json:"provider"`
	AmountMinor int64  `json:"amount_minor"`
	Note        string `json:"note"`
	Raw         string `json:"raw"`
	Status      string `json:"status"` // pending / resolved / refunded
	Handler     string `json:"handler"`
	// 风控留痕（2026-09-14 需求：无订单收款不做熔断——直发码无法阻止对方转账；
	// 记录身份→行为标记→告警提示，保留司法可核对记录）
	Payer     string    `json:"payer"`      // 付款人（账本行脱敏名；通知栏无则空）
	DeviceID  string    `json:"device_id"`  // 收款设备（锁定收款码归属）
	RiskTags  string    `json:"risk_tags"`  // 逗号分隔：big_amount/burst/repeat_payer
	RiskLevel int       `json:"risk_level"` // 0 正常 / 1 疑似核实 / 2 高风险
	CreatedAt time.Time `json:"created_at"`
}

// DeviceQrcode 设备动态收款码（服务端 adb 捕获支付宝/微信收款码页）
type DeviceQrcode struct {
	ID           int64  `json:"id"`
	DeviceID     string `json:"device_id"`
	Platform     string `json:"platform"`      // alipay / wechat
	UserID       int    `json:"user_id"`       // Android 用户空间：0=主空间，999=双开(XSpace)
	QRContent    string `json:"qr_content"`    // 解码后的二维码内容（URL 或串）
	Account      string `json:"account"`       // 收款账户标识（昵称/微信号）
	ExpireIn     int64  `json:"expire_in"`     // 码有效期（秒）
	ExpireAt     string `json:"expire_at"`     // 过期时间（RFC3339）
	Status       string `json:"status"`        // active / expired
	PendingFetch int64  `json:"pending_fetch"` // 服务端待下发 fetch_qrcode 指令标志（1=待触发）
	CreatedAt    string `json:"created_at"`
}

// UserSpaceLabel 返回用户空间的显示标签
func (q *DeviceQrcode) UserSpaceLabel() string {
	if q.UserID == 999 {
		return "双开" // 小米 XSpace 分身
	}
	return "主空间"
}

// ParseSetupProgress 把 SetupProgress 字段（DB 存的 JSON 数组字符串）解析为有序切片。// 空串或非法 JSON 视为空切片（不报错，半配置态守恒语义靠心跳 handler 不解冻账户兜底）。
func ParseSetupProgress(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// FormatSetupProgress 把字符串切片格式化为 JSON 数组字符串入库。
func FormatSetupProgress(steps []string) string {
	if len(steps) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(steps)
	return string(b)
}

// HasSetupStep 返回 steps 是否包含 step。
func HasSetupStep(steps []string, step string) bool {
	for _, s := range steps {
		if s == step {
			return true
		}
	}
	return false
}

// AppendSetupStep 幂等追加 step 到 steps（已含则原样返回切片）。
func AppendSetupStep(steps []string, step string) []string {
	if HasSetupStep(steps, step) {
		return steps
	}
	return append(steps, step)
}
