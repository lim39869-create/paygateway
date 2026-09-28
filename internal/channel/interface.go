// Package channel 支付通道抽象：统一接口 + 注册表，通道可插拔
package channel

import (
	"context"
	"net/http"
	"time"
)

// ChannelType 通道大类
type ChannelType string

const (
	// TypeMerchant 商家通道：官方 API，gopay 驱动，可主动查单
	TypeMerchant ChannelType = "merchant"
	// TypePersonal 个人通道：免签收款码，监听驱动，无法主动查单
	TypePersonal ChannelType = "personal"
)

// Provider 支付品牌（两类通道共用）
type Provider string

const (
	ProviderAlipay Provider = "alipay"
	ProviderWechat Provider = "wechat"
	ProviderQQ     Provider = "qq"   // 预留
	ProviderUSDT   Provider = "usdt" // 预留
)

// ChannelKind 通道类型（支付结果获取方式）
type ChannelKind int

const (
	// KindCallback 回调驱动：支付宝/微信等（用户付款 → 通道异步回调）
	KindCallback ChannelKind = iota
	// KindListener 监听驱动：链上支付/免签监听（用户转账 → 监听确认）
	KindListener
)

// ConfirmAbility 确认能力：决定 ConfirmScheduler 的兜底策略
type ConfirmAbility int

const (
	// ConfirmByQuery 商家通道：可主动调 API 查单（回调丢失→查单补账）
	ConfirmByQuery ConfirmAbility = iota
	// ConfirmByListen 个人通道：靠监听端推送，无法主动查
	ConfirmByListen
	// ConfirmByManual 最终兜底：监听失联/匹配失败→人工异常池
	ConfirmByManual
)

// PayMode 支付模式
type PayMode string

const (
	ModePage    PayMode = "page"    // 电脑网站支付（跳转收银台）
	ModeWap     PayMode = "wap"     // 手机网站支付
	ModeQRCode  PayMode = "qrcode"  // 扫码支付（当面付）
	ModeAddress PayMode = "address" // 链上收款地址（未来 crypto）
)

// PayRequest 下单请求（通道无关）
type PayRequest struct {
	OutTradeNo  string  // 网关订单号
	GatewayNo   string  // 网关单号
	Subject     string  // 商品名称
	AmountMinor int64   // 金额（最小单位）
	Currency    string  // 币种
	NotifyURL   string  // 通道异步回调地址
	ReturnURL   string  // 跳转地址
	PayMode     PayMode // 用户选择的支付模式
	Device      string  // pc / mobile（用于默认模式判断）
}

// PayCredential 支付凭据（下单返回）
type PayCredential struct {
	PayURL    string    // 跳转链接（page/wap）
	QRCode    string    // 二维码内容（qrcode/address）
	Address   string    // 链上收款地址（未来 crypto）
	AccountID int64     // 处理这笔订单的账户ID（账户池用）
	Mode      PayMode   // 实际使用的支付模式（含降级后的结果）
	ExpireAt  time.Time // 过期时间

	// 个人免签通道附加数据（由通道返回，order.Service 持久化到订单）
	NoteCode  string // 备注码（用户转账备注）
	QrToken   string // 套码 token（/mq/{token} 校验）
	QRContent string // 真实收款码内容（仅服务端使用，302 跳转目标）
	DeviceID  string // 承接的监听端设备（匹配后回填）
}

// NotifyResult 回调/监听事件解析结果
type NotifyResult struct {
	Valid       bool   // 验签/校验是否通过
	OutTradeNo  string // 商户订单号（通道回调携带）
	ChannelTxID string // 通道交易号（去重依据）
	Status      string // 与 model 状态一致：TRADE_SUCCESS / TRADE_CLOSED / FAILED
	AmountMinor int64  // 实付金额（必须与订单比对！）
	Payer       string // 付款方标识（可选）
	DeviceID    string // 个人通道：承接的监听端设备ID（审计追溯用）
	AppID       string // 支付宝 app_id（回调反查账户用）
	MchID       string // 微信商户号（回调反查账户用）
	AccountID   int64  // 处理这笔回调的账户ID（留痕/审计）
	RawPayload  string // 原始数据（审计用）
}

// OrderStatus 主动查询结果
type OrderStatus struct {
	ChannelTxID string
	Status      string
	AmountMinor int64
}

// EventListener 事件监听器（链上支付用；回调型通道返回 nil）
type EventListener interface {
	Start(ctx context.Context) error
	Events() <-chan ChainEvent
}

// ChainEvent 链上事件
type ChainEvent struct {
	ChannelTxID   string
	Address       string
	AmountMinor   int64
	Confirmations int64
	RawPayload    string
}

// ClientCacheInvalidator 可清除账户客户端缓存的可选接口。
// 商家通道（alipay/wechat）持有 gopay client 缓存；账户 app_id/密钥变更后必须清除，
// 否则缓存的旧 client 会继续用旧参数下单（如旧 app_id 导致 invalid-app-id）。
type ClientCacheInvalidator interface {
	RemoveClient(accountID int64)
}

// PaymentChannel 所有支付通道必须实现的统一接口
type PaymentChannel interface {
	// Name 通道标识：merchant.alipay / personal.wechat 等
	Name() string
	// Type 通道大类：merchant / personal
	Type() ChannelType
	// Kind 通道类型（回调驱动 / 监听驱动）
	Kind() ChannelKind
	// Modes 支持的支付模式
	Modes() []PayMode
	// ConfirmAbility 确认能力（决定 ConfirmScheduler 的兜底策略）
	ConfirmAbility() ConfirmAbility
	// Provider 支付品牌：alipay / wechat / qq / usdt
	Provider() Provider
	// CreatePayment 下单：返回支付凭据
	CreatePayment(ctx context.Context, req *PayRequest) (*PayCredential, error)
	// VerifyNotify 验签回调（KindCallback 用）：解析 + 验签原始请求
	VerifyNotify(ctx context.Context, req *http.Request) (*NotifyResult, error)
	// QueryOrder 主动查单（对账 + 补偿兜底）
	QueryOrder(ctx context.Context, outTradeNo string) (*OrderStatus, error)
	// Listener 事件监听（KindListener 必须实现；KindCallback 返回 nil）
	Listener() EventListener
}
