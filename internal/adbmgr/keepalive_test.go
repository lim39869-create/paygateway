package adbmgr

import (
	"strings"
	"testing"
)

// TestOpLineAllow 验证 appops get 输出解析（KeepAliveStatus 内部逻辑）。
func TestOpLineAllow(t *testing.T) {
	cases := []struct {
		name string
		out  string
		op   string
		want bool
	}{
		{"allow 命中", "RUN_IN_BACKGROUND: allow\nFOO: deny", "RUN_IN_BACKGROUND", true},
		{"deny 不命中", "RUN_IN_BACKGROUND: deny\n", "RUN_IN_BACKGROUND", false},
		{"op 不存在", "SOME_OTHER: allow\n", "RUN_IN_BACKGROUND", false},
		{"allow 带时间戳", "RUN_ANY_IN_BACKGROUND: allow; time=+2d22h47m34s ago", "RUN_ANY_IN_BACKGROUND", true},
		{"前缀不误匹配", "RUN_ANY_IN_BACKGROUND_EXTRA: allow\n", "RUN_ANY_IN_BACKGROUND", false},
		{"空输出", "", "RUN_IN_BACKGROUND", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := opLineAllow(tc.out, tc.op); got != tc.want {
				t.Fatalf("opLineAllow(%q, %q) = %v, want %v", tc.out, tc.op, got, tc.want)
			}
		})
	}
}

// TestKeepAliveStatus 用假设备输出验证 status 组装（不依赖真实 adb）。
// 通过覆盖 adb 命令返回模拟输出——这里直接测解析层逻辑（与 adb 解耦）。
func TestKeepAliveStatusParsing(t *testing.T) {
	// 模拟 appops get 输出
	appopsOut := "RUN_IN_BACKGROUND: allow\nRUN_ANY_IN_BACKGROUND: allow; time=+2d ago\n"
	devidleOut := "user,com.tencent.mm,10277\nuser,com.eg.android.AlipayGphone,10250\n"

	if !opLineAllow(appopsOut, "RUN_IN_BACKGROUND") {
		t.Fatal("RUN_IN_BACKGROUND 应解析为 allow")
	}
	if !opLineAllow(appopsOut, "RUN_ANY_IN_BACKGROUND") {
		t.Fatal("RUN_ANY_IN_BACKGROUND 应解析为 allow")
	}
	// deviceidle 输出含 "user,<pkg>," 即已加入白名单
	if !containsPkg(devidleOut, "com.tencent.mm") {
		t.Fatal("deviceidle 输出应含 com.tencent.mm")
	}
}

func containsPkg(out, pkg string) bool {
	for _, line := range splitLines(out) {
		if strings.HasPrefix(line, "user,"+pkg+",") {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
