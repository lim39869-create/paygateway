package account

import "strings"

// InvalidQRContent 校验账户收款码内容格式。返回非空字符串 = 非法原因（"" = 合法）。
// 目的（资金安全防线）：QRContent 直接决定扫码后用户的支付去向，脏数据（如把报错
// 文案/任意文本误存为码）会生成一个无法支付的二维码，直接毁掉支付体验。
// 合法格式（对齐两平台实际码结构）：
//   - 支付宝（personal.alipay）：https:// 开头的收款码 URL
//   - 微信（personal.wechat）：wxp:// 或 weixin:// 开头的私有收款协议
//
// 两平台都要求内容非空且不含空白/控制字符（QR 内容带换行/空格会编码异常）。
// channel 形如 "personal.alipay" / "personal.wechat"；空/未知通道按 https URL 要求。
func InvalidQRContent(channel, content string) string {
	if content == "" {
		return "内容为空"
	}
	// 内容中任何位置都不允许空白/控制字符（QR 编码空格/换行会变异，扫码后内容不符）
	if strings.IndexFunc(content, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\r' || r == '\n'
	}) >= 0 {
		return "内容含空白或控制字符"
	}
	if strings.HasPrefix(channel, "personal.wechat") {
		if strings.HasPrefix(content, "wxp://") || strings.HasPrefix(content, "weixin://") {
			return ""
		}
		return "微信收款码必须以 wxp:// 或 weixin:// 开头"
	}
	if !strings.HasPrefix(content, "https://") {
		return "收款码必须为 https:// URL"
	}
	return ""
}
