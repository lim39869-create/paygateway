// Package upstream 上游对接方管理：多商户（易支付 pid+key）独立加密存储
//
// 安全设计：
//   - upstreams.db 仅管理后台 API 可读写，其他模块不得直接访问
//   - 敏感字段（易支付密钥 key）AES-256-GCM 加密存储（复用 account 包的加密能力）
//   - UpstreamPool 是内存中的唯一数据源，其他模块（epay 验签/回调签名）通过它获取解密后的密钥
//   - 每个上游独立 pid + key：下单验签、回调签名均按 pid 路由，互不伪造
package upstream

import "time"

// UpstreamStatus 上游状态
type UpstreamStatus string

const (
	StatusEnabled  UpstreamStatus = "enabled"  // 启用：可下单、可回调
	StatusDisabled UpstreamStatus = "disabled" // 禁用：拒绝下单
)

// Upstream 上游对接方（内存中的完整表示，含解密后的密钥）
// upstreams.db 中存储的是加密版本，Admin API 解密后推送到 UpstreamPool
type Upstream struct {
	ID            int64          `json:"id"`
	Name          string         `json:"name"`           // 上游名称："new-api"
	Pid           string         `json:"pid"`            // 易支付商户ID（验签/签名依据，唯一）
	Key           string         `json:"-"`              // 易支付密钥（仅内存，不落盘明文）
	BaseURL       string         `json:"base_url"`       // 对外服务地址（该上游配置用的地址）
	NotifyDomains string         `json:"notify_domains"` // 回调目标域名白名单（逗号分隔，可选）
	Status        UpstreamStatus `json:"status"`         // enabled / disabled
	Remark        string         `json:"remark,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
}

// UpstreamEncrypted upstreams.db 中存储的加密版本
type UpstreamEncrypted struct {
	ID            int64     `json:"id"`
	Name          string    `json:"name"`
	Pid           string    `json:"pid"`
	KeyEnc        string    `json:"key_enc"` // AES-256-GCM 加密后的 hex 密文
	BaseURL       string    `json:"base_url"`
	NotifyDomains string    `json:"notify_domains"`
	Status        string    `json:"status"`
	Remark        string    `json:"remark,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// UpstreamLog 上游操作审计日志
type UpstreamLog struct {
	ID         int64     `json:"id"`
	UpstreamID int64     `json:"upstream_id"`
	Action     string    `json:"action"`   // create / update / delete / enable / disable
	Detail     string    `json:"detail"`   // 操作详情
	Operator   string    `json:"operator"` // 操作人
	CreatedAt  time.Time `json:"created_at"`
}
