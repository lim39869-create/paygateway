package notify

import (
	"net"
	"testing"
)

func TestValidateNotifyURL(t *testing.T) {
	tests := []struct {
		name         string
		raw          string
		allowPrivate bool
		wantErr      bool
	}{
		{"空地址合法", "", true, false},
		{"空地址私网封锁下仍合法", "", false, false},
		{"公网 http(IP字面量)", "http://8.8.4.4/notify", false, false},
		{"公网 https(IP字面量)", "https://[2606:4700:4700::1111]/notify.php", false, false},
		{"ftp 协议拒绝", "ftp://example.com/x", true, true},
		{"javascript 协议拒绝", "javascript:alert(1)", true, true},
		{"缺主机名", "http:///path", true, true},
		{"环回封禁", "http://127.0.0.1:3000/api/notify", false, true},
		{"环回放行", "http://127.0.0.1:3000/api/notify", true, false},
		{"localhost 域名封禁", "http://localhost:8080/notify", false, true},
		{"内网 IP 封禁", "http://192.168.1.10:8080/notify", false, true},
		{"链路本地封禁(云元数据)", "http://169.254.169.254/latest/meta-data", false, true},
		{"Tailscale CGNAT 段封禁", "http://100.64.0.1/notify", false, true},
		{"内网 IP 放行", "http://192.168.1.10:8080/notify", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNotifyURL(tt.raw, tt.allowPrivate)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateNotifyURL(%q, %v) err=%v, wantErr=%v", tt.raw, tt.allowPrivate, err, tt.wantErr)
			}
		})
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":        true,
		"10.1.2.3":         true,
		"172.16.0.1":       true,
		"172.31.255.255":   true,
		"172.32.0.1":       false,
		"192.168.0.1":      true,
		"169.254.169.254":  true,
		"100.64.0.1":       true,
		"8.8.8.8":          false,
		"1.1.1.1":          false,
		"::1":              true,
		"fe80::1":          true,
		"fd00::1":          true,
		"2606:4700:4700::": false,
	}
	for ip, want := range cases {
		if got := isPrivateIP(net.ParseIP(ip)); got != want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", ip, got, want)
		}
	}
}
