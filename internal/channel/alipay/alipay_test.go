package alipay

import (
	"testing"

	"github.com/go-pay/gopay/alipay"
	"paygateway/internal/account"
	"paygateway/internal/channel"
)

// newTestChannel 创建测试通道（默认全开）
func newTestChannel() *Channel {
	ch := &Channel{
		pool:         &account.AccountPool{},
		clientCache:  make(map[int64]*alipay.Client),
		enabledModes: defaultEnabledModes(),
	}
	return ch
}

func TestResolveMode_AllEnabled(t *testing.T) {
	ch := newTestChannel()

	cases := []struct {
		name     string
		payMode  string
		device   string
		expected channel.PayMode
	}{
		{"显式指定 page", "page", "pc", channel.ModePage},
		{"显式指定 wap", "wap", "mobile", channel.ModeWap},
		{"显式指定 qrcode", "qrcode", "pc", channel.ModeQRCode},
		{"pc 设备识别", "", "pc", channel.ModePage},
		{"page 设备识别", "", "page", channel.ModePage},
		{"mobile 设备识别", "", "mobile", channel.ModeWap},
		{"wap 设备识别", "", "wap", channel.ModeWap},
		{"未知设备默认扫码", "", "unknown", channel.ModeQRCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &channel.PayRequest{PayMode: channel.PayMode(tc.payMode), Device: tc.device}
			got := ch.resolveMode(req)
			if got != tc.expected {
				t.Fatalf("resolveMode(%q, %q) = %q, want %q", tc.payMode, tc.device, got, tc.expected)
			}
		})
	}
}

func TestResolveMode_OnlyQRCode(t *testing.T) {
	ch := newTestChannel()
	ch.SetModeEnabled("page", false)
	ch.SetModeEnabled("wap", false)
	// 只开 qrcode

	cases := []struct {
		name     string
		payMode  string
		device   string
		expected channel.PayMode
	}{
		{"pc 降级到 qrcode", "", "pc", channel.ModeQRCode},
		{"mobile 降级到 qrcode", "", "mobile", channel.ModeQRCode},
		{"未知设备降级到 qrcode", "", "unknown", channel.ModeQRCode},
		{"显式指定已关闭的 page 降级", "page", "pc", channel.ModeQRCode},
		{"显式指定已关闭的 wap 降级", "wap", "mobile", channel.ModeQRCode},
		{"显式指定 qrcode 保持", "qrcode", "pc", channel.ModeQRCode},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &channel.PayRequest{PayMode: channel.PayMode(tc.payMode), Device: tc.device}
			got := ch.resolveMode(req)
			if got != tc.expected {
				t.Fatalf("resolveMode(%q, %q) = %q, want %q", tc.payMode, tc.device, got, tc.expected)
			}
		})
	}
}

func TestResolveMode_OnlyWap(t *testing.T) {
	ch := newTestChannel()
	ch.SetModeEnabled("page", false)
	ch.SetModeEnabled("qrcode", false)
	// 只开 wap

	cases := []struct {
		name     string
		payMode  string
		device   string
		expected channel.PayMode
	}{
		{"pc 降级到 wap", "", "pc", channel.ModeWap},
		{"mobile 保持 wap", "", "mobile", channel.ModeWap},
		{"显式指定已关闭的 page 降级", "page", "pc", channel.ModeWap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &channel.PayRequest{PayMode: channel.PayMode(tc.payMode), Device: tc.device}
			got := ch.resolveMode(req)
			if got != tc.expected {
				t.Fatalf("resolveMode(%q, %q) = %q, want %q", tc.payMode, tc.device, got, tc.expected)
			}
		})
	}
}

func TestResolveMode_OnlyPage(t *testing.T) {
	ch := newTestChannel()
	ch.SetModeEnabled("wap", false)
	ch.SetModeEnabled("qrcode", false)
	// 只开 page

	cases := []struct {
		name     string
		payMode  string
		device   string
		expected channel.PayMode
	}{
		{"pc 保持 page", "", "pc", channel.ModePage},
		{"mobile 降级到 page", "", "mobile", channel.ModePage},
		{"显式指定已关闭的 wap 降级", "wap", "mobile", channel.ModePage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &channel.PayRequest{PayMode: channel.PayMode(tc.payMode), Device: tc.device}
			got := ch.resolveMode(req)
			if got != tc.expected {
				t.Fatalf("resolveMode(%q, %q) = %q, want %q", tc.payMode, tc.device, got, tc.expected)
			}
		})
	}
}

func TestResolveMode_AllDisabled(t *testing.T) {
	ch := newTestChannel()
	ch.SetModeEnabled("page", false)
	ch.SetModeEnabled("wap", false)
	ch.SetModeEnabled("qrcode", false)

	// 全部关闭：fallback 返回设备首选（pc→page），由 CreatePayment 报错
	req := &channel.PayRequest{PayMode: "", Device: "pc"}
	got := ch.resolveMode(req)
	if got != channel.ModePage {
		t.Fatalf("resolveMode(pc, all disabled) = %q, want %q", got, channel.ModePage)
	}
	// 全部关闭时 CreatePayment 必须报错
	if _, err := ch.CreatePayment(nil, &channel.PayRequest{Device: "pc", Subject: "t", OutTradeNo: "x", AmountMinor: 100, Currency: "CNY"}); err == nil {
		t.Fatal("全模式关闭时 CreatePayment 应返回错误")
	}
}

func TestEnabledModesSnapshot(t *testing.T) {
	ch := newTestChannel()
	ch.SetModeEnabled("wap", false)
	snapshot := ch.EnabledModes()
	// 修改快照不应影响内部状态
	snapshot["wap"] = true
	if ch.IsModeEnabled("wap") {
		t.Fatal("修改快照不应影响内部状态")
	}
}
