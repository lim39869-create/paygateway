// Package model 领域模型：订单、状态常量、金额类型
package model

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 订单状态（状态机）
const (
	StatusCreated    = "CREATED"        // 已创建
	StatusWaitPay    = "WAIT_BUYER_PAY" // 等待支付（与易支付协议保持一致）
	StatusConfirming = "CONFIRMING"     // 链上确认中（未来 crypto 用）
	StatusSuccess    = "TRADE_SUCCESS"  // 支付成功（与易支付协议保持一致）
	StatusClosed     = "TRADE_CLOSED"   // 已关闭/超时（与易支付协议保持一致）
	StatusFailed     = "FAILED"         // 支付失败（链上重组等）
)

// 订单状态允许的流转（防止状态回跳/非法跳转）
var allowedTransitions = map[string][]string{
	StatusCreated:    {StatusWaitPay, StatusClosed},
	StatusWaitPay:    {StatusSuccess, StatusClosed, StatusConfirming},
	StatusConfirming: {StatusSuccess, StatusFailed, StatusClosed},
	StatusSuccess:    {}, // 终态，不可再变
	StatusClosed:     {}, // 终态
	StatusFailed:     {}, // 终态
}

// CanTransition 判断状态流转是否合法
func CanTransition(from, to string) bool {
	if from == to {
		return true // 幂等
	}
	for _, next := range allowedTransitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// Order 订单领域模型
type Order struct {
	ID               int64      `json:"id"`
	OutTradeNo       string     `json:"out_trade_no"`            // 接入方订单号（幂等依据）
	GatewayNo        string     `json:"gateway_no"`              // 网关订单号
	Channel          string     `json:"channel"`                 // alipay / wechat / paypal / usdt
	ChannelTxID      string     `json:"channel_tx_id"`           // 通道交易号（回调去重）
	AccountID        int64      `json:"account_id"`              // 处理这笔订单的账户ID（账户池）
	Pid              string     `json:"pid"`                     // 上游易支付商户ID（多商户路由，空=default）
	UpstreamName     string     `json:"upstream_name,omitempty"` // 上游名称（来源标识）
	Attach           string     `json:"attach,omitempty"`        // 上游请求ID（易支付 attach 字段，原样透传，用于对账）
	AccountName      string     `json:"account_name,omitempty"`  // 账户名称（展示用，查询时附加）
	AmountMinor      int64      `json:"amount_minor"`            // 金额（最小单位：分）
	Currency         string     `json:"currency"`                // CNY / USD / USDT
	Subject          string     `json:"subject"`                 // 商品名称
	Status           string     `json:"status"`                  // 状态机
	NotifyURL        string     `json:"notify_url"`              // 回调地址（接入方）
	ReturnURL        string     `json:"return_url"`              // 跳转地址
	QrCode           string     `json:"qr_code"`                 // 二维码内容
	PayURL           string     `json:"pay_url"`                 // 支付链接/凭据
	Payer            string     `json:"payer"`                   // 付款方标识（可选）
	NoteCode         string     `json:"note_code,omitempty"`     // 个人免签：备注码
	QrToken          string     `json:"qr_token,omitempty"`      // 个人免签：套码 token
	DeviceID         string     `json:"device_id,omitempty"`     // 个人免签：承接的监听端设备
	BuyerKey         string     `json:"buyer_key,omitempty"`     // 下单环境唯一标识（IP+UA+设备指纹 hash，买家级串行/防刷单用）
	FPKey            string     `json:"fp_key,omitempty"`        // 浏览器指纹标识（canvas/时区/屏幕 hash，支付页 JS 上报，抗换 IP）
	Notified         bool       `json:"notified"`                // 是否已通知接入方
	CreatedAt        time.Time  `json:"created_at"`
	SessionExpiresAt time.Time  `json:"session_expires_at,omitempty"`
	PaidAt           *time.Time `json:"paid_at,omitempty"`
	NotifiedAt       *time.Time `json:"notified_at,omitempty"`
	NotifyCount      int        `json:"notify_count"` // 通知次数（outbox 重试用）

	// 人工异常处理标记（人工池处理后永久留痕）
	ExceptionID   int64  `json:"exception_id"`
	HandledBy     string `json:"handled_by"`
	HandledAction string `json:"handled_action"` // manual_settle / manual_close / noop
	HandledAt     string `json:"handled_at"`
	HandledNote   string `json:"handled_note"`
}

// 兼容易支付协议：金额为字符串"元"，需要与最小单位互转

// MoneyYuan 返回金额（元，字符串，易支付协议格式）
func (o *Order) MoneyYuan() string {
	if o.AmountMinor <= 0 {
		return "0.00"
	}
	return fmt.Sprintf("%d.%02d", o.AmountMinor/100, o.AmountMinor%100)
}

// ParseMoneyToMinor 将金额字符串（元）解析为最小单位（分）
// 支持 "0.01"、"10"、"10.5"、"10.50" 等格式；非法返回 error
func ParseMoneyToMinor(money string) (int64, error) {
	money = strings.TrimSpace(money)
	if money == "" {
		return 0, fmt.Errorf("金额为空")
	}
	// 拆分整数和小数部分
	parts := strings.Split(money, ".")
	intPart := parts[0]
	fracPart := ""
	if len(parts) > 1 {
		fracPart = parts[1]
		if len(parts) > 2 {
			return 0, fmt.Errorf("金额格式非法: %s", money)
		}
	}
	// 整数部分
	whole, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil || whole < 0 {
		return 0, fmt.Errorf("金额整数部分非法: %s", money)
	}
	// 小数部分：最多 2 位，补零到 2 位
	if len(fracPart) > 2 {
		return 0, fmt.Errorf("金额最多两位小数: %s", money)
	}
	frac := fracPart + strings.Repeat("0", 2-len(fracPart))
	fracInt := int64(0)
	if frac != "" {
		fracInt, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("金额小数部分非法: %s", money)
		}
	}
	minor := whole*100 + fracInt
	if minor <= 0 {
		return 0, fmt.Errorf("金额必须大于 0: %s", money)
	}
	return minor, nil
}
