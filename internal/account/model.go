// Package account 账户池：多支付账户管理 + 加密存储 + 调度器
//
// 安全设计：
//   - accounts.db 仅管理后台 API 可读写，其他模块不得直接访问
//   - 敏感字段（private_key / public_key / api_key）AES-256-GCM 加密存储
//   - AccountPool 是内存中的唯一数据源，其他模块通过它获取解密后的账户
package account

import "time"

// AccountStatus 账户状态
type AccountStatus string

const (
	StatusNormal   AccountStatus = "normal"   // 正常
	StatusCooling  AccountStatus = "cooling"  // 风控冷却中（自动恢复）
	StatusFrozen   AccountStatus = "frozen"   // 冻结（按 FrozenReason 区分来源）
	StatusDisabled AccountStatus = "disabled" // 永久禁用
)

// 账户冻结原因（StatusFrozen 不区分来源，用 FrozenReason 精确控制自动解冻边界）。
// 只有 FrozenReasonOffline 允许设备恢复时自动解冻；其余一律仅人工解冻。
const (
	FrozenReasonOffline = "offline" // 设备离线联动：设备 adb 恢复可达时自动解冻
	FrozenReasonRisk    = "risk"    // 风控上报冻结：仅人工解冻
	FrozenReasonManual  = "manual"  // 管理员手动冻结：仅人工解冻
	FrozenReasonRetire  = "retire"  // 删除设备级联停用：仅人工解冻
)

// Account 支付账户（内存中的完整表示，含解密后的密钥）
// accounts.db 中存储的是加密版本，Admin API 解密后推送到 AccountPool
type Account struct {
	ID      int64  `json:"id"`
	Channel string `json:"channel"`          // alipay / wechat / qq / paypal / personal.alipay / personal.wechat
	Name    string `json:"name"`             // "支付宝-主账户"
	AppID   string `json:"app_id"`           // 明文，用于回调反查
	MchID   string `json:"mch_id,omitempty"` // 微信商户号

	// 以下字段在 accounts.db 中加密存储，AccountPool 中为明文
	PrivateKey string `json:"-"` // 应用私钥（仅内存）
	PublicKey  string `json:"-"` // 支付宝公钥/微信API证书（仅内存）
	ApiKey     string `json:"-"` // API 密钥（仅内存）
	QRContent  string `json:"-"` // 收款码内容（个人免签通道用，仅内存）

	// 调度配置
	Weight       int           `json:"weight"`                  // 轮询权重（越大越容易被选中）
	Priority     int           `json:"priority"`                // 优先级（越大越优先）
	Status       AccountStatus `json:"status"`                  // normal / cooling / frozen / disabled
	FrozenReason string        `json:"frozen_reason,omitempty"` // 冻结原因（offline / risk / manual / retire；仅 frozen 时有效）
	AutoBan      bool          `json:"auto_ban"`                // 失败自动禁用
	DailyLimit   int64         `json:"daily_limit"`             // 日限额（分），0=不限
	DailyUsed    int64         `json:"daily_used"`              // 今日已用（分）
	FailCount    int           `json:"fail_count"`              // 连续失败次数
	FrozenUntil  *time.Time    `json:"frozen_until,omitempty"`  // 冷却到期时间
	Group        string        `json:"group"`                   // 业务分组
	IsProd       bool          `json:"is_prod"`                 // 生产环境
	Remark       string        `json:"remark,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AccountEncrypted accounts.db 中存储的加密版本
// 敏感字段以 hex 密文存储，非敏感字段明文
type AccountEncrypted struct {
	ID      int64  `json:"id"`
	Channel string `json:"channel"`
	Name    string `json:"name"`
	AppID   string `json:"app_id"`
	MchID   string `json:"mch_id,omitempty"`

	// AES-256-GCM 加密后的 hex 密文
	PrivateKeyEnc string `json:"private_key_enc"`
	PublicKeyEnc  string `json:"public_key_enc,omitempty"`
	ApiKeyEnc     string `json:"api_key_enc,omitempty"`
	QRContentEnc  string `json:"qr_content_enc,omitempty"` // 收款码内容（个人免签通道）

	Weight       int       `json:"weight"`
	Priority     int       `json:"priority"`
	Status       string    `json:"status"`
	FrozenReason string    `json:"frozen_reason,omitempty"` // 冻结原因（offline / risk / manual / retire）
	AutoBan      bool      `json:"auto_ban"`
	DailyLimit   int64     `json:"daily_limit"`
	DailyUsed    int64     `json:"daily_used"` // 今日已用（分），持久化（每日 00:00 重置）
	Group        string    `json:"group"`
	IsProd       bool      `json:"is_prod"`
	Remark       string    `json:"remark,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// AccountLog 账户操作审计日志
type AccountLog struct {
	ID        int64     `json:"id"`
	AccountID int64     `json:"account_id"`
	Action    string    `json:"action"`   // create / update / delete / freeze / unfreeze / enable / disable
	Detail    string    `json:"detail"`   // 操作详情
	Operator  string    `json:"operator"` // 操作人
	CreatedAt time.Time `json:"created_at"`
}
