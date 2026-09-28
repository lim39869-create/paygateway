package admin

import (
	"strings"
	"testing"
)

// TestKeepaliveHintApps 保活引导文案的支付通道应用映射。
// 2026-08-30：监听端 APP 弃用，保活引导只包含支付通道 APP。
func TestKeepaliveHintApps(t *testing.T) {
	cases := []struct {
		provider    string
		wantApp     string // 期望文案包含的支付 APP 显示名
		wantChannel string // 期望的通道显示名（空=无）
		count       int
	}{
		{"wechat", "「微信」", "微信", 1},
		{"alipay", "「支付宝」", "支付宝", 1},
		{"unknown", "", "", 1},
		{"", "", "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.provider, func(t *testing.T) {
			apps := keepaliveHintApps(tc.provider)
			if tc.wantApp != "" && !strings.Contains(apps, tc.wantApp) {
				t.Fatalf("keepaliveHintApps(%q)=%q 应包含 %q", tc.provider, apps, tc.wantApp)
			}
			if strings.Contains(apps, "监听端") {
				t.Fatalf("keepaliveHintApps(%q)=%q 不应再包含监听端 APP", tc.provider, apps)
			}
			if got := keepaliveHintAppsCount(tc.provider); got != tc.count {
				t.Fatalf("keepaliveHintAppsCount(%q)=%d, want %d", tc.provider, got, tc.count)
			}
			if got := keepaliveChannelApp(tc.provider); got != tc.wantChannel {
				t.Fatalf("keepaliveChannelApp(%q)=%q, want %q", tc.provider, got, tc.wantChannel)
			}
		})
	}
}
