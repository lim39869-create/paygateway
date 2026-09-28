package web

import (
	"strings"
	"testing"
	"time"

	"paygateway/internal/model"
)

func TestBuildPersonalQRPage_NoteCode(t *testing.T) {
	o := &model.Order{OutTradeNo: "T1", NoteCode: "1234", AmountMinor: 1000}
	html := BuildPersonalQRPage(o, "/qr/real?token=x", "personal.alipay", time.Now().Add(5*time.Minute))
	if !strings.Contains(html, "1234") {
		t.Fatalf("支付页应包含备注码 1234")
	}
	if !strings.Contains(html, "请备注") {
		t.Fatalf("支付页应包含备注提示文案")
	}
	// 无备注码时不渲染该区块
	o2 := &model.Order{OutTradeNo: "T2", AmountMinor: 1000}
	h2 := BuildPersonalQRPage(o2, "/qr/real?token=y", "personal.alipay", time.Now())
	if strings.Contains(h2, "请备注") {
		t.Fatalf("无备注码订单不应渲染备注提示")
	}
}

func TestBuildSuccessPage(t *testing.T) {
	paidAt := time.Date(2026, 9, 12, 10, 30, 0, 0, time.Local)
	o := &model.Order{OutTradeNo: "S1", AmountMinor: 2350, Channel: "personal.alipay", ReturnURL: "https://merchant.example/order/1", PaidAt: &paidAt}
	html := BuildSuccessPage(o, o.Channel)
	for _, want := range []string{"支付成功", "已支付成功", "S1", "23.50", "2026-09-12 10:30:00", "返回商户"} {
		if !strings.Contains(html, want) {
			t.Fatalf("成功页应包含 %q", want)
		}
	}
	// 无 return_url 时不渲染返回商户按钮
	o2 := &model.Order{OutTradeNo: "S2", AmountMinor: 100}
	h2 := BuildSuccessPage(o2, "merchant.wechat")
	if strings.Contains(h2, "返回商户") {
		t.Fatalf("无 return_url 订单不应渲染返回商户按钮")
	}
	if !strings.Contains(h2, "微信支付成功") {
		t.Fatalf("微信通道成功页应显示微信品牌文案")
	}
}

func TestBuildQRPage_HasSuccessPolling(t *testing.T) {
	o := &model.Order{OutTradeNo: "TQ1", AmountMinor: 100}
	html := BuildQRPage(o, "alipay-content", "merchant.alipay")
	for _, want := range []string{"/qr/status?out_trade_no=", "TRADE_SUCCESS", "document.write"} {
		if !strings.Contains(html, want) {
			t.Fatalf("商家二维码页应包含支付成功轮询 %q", want)
		}
	}
}

func TestBuildPersonalQRPage_HasSuccessPolling(t *testing.T) {
	o := &model.Order{OutTradeNo: "TP1", AmountMinor: 100, NoteCode: "1234"}
	html := BuildPersonalQRPage(o, "/qr/real?token=x", "personal.alipay", time.Now().Add(5*time.Minute))
	for _, want := range []string{"/qr/status?out_trade_no=", "TRADE_SUCCESS", "document.write"} {
		if !strings.Contains(html, want) {
			t.Fatalf("个人免签支付页应包含支付成功轮询 %q", want)
		}
	}
}
