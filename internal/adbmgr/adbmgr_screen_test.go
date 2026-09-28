package adbmgr

import (
	"testing"
)

// TestParseWmSize 验证 `adb shell wm size` 输出解析（ScreenSize 内部逻辑，与 adb 解耦）。
// 覆盖：Override/Physical/裸格式三种来源、优先级（Override 优先——input/dump 坐标基准）、
// 空白/换行容错、非法与空输出。
func TestParseWmSize(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		wantW   int
		wantH   int
		wantErr bool
	}{
		{"Physical 标准格式", "Physical size: 1080x2400\n", 1080, 2400, false},
		{"Override 标准格式", "Override size: 720x1600\n", 720, 1600, false},
		{"裸格式", "1440x3200", 1440, 3200, false},
		{"Override 优先于 Physical", "Physical size: 1080x2400\nOverride size: 720x1600\n", 720, 1600, false},
		{"红米K60 双尺寸实测", "Physical size: 1440x3200\nOverride size: 1080x2400\n", 1080, 2400, false},
		{"Physical 在第二行", "Override size: 720x1600\nPhysical size: 1080x2400\n", 720, 1600, false},
		{"前后多余空白", "  \nOverride size: 720x1600  \n  \n", 720, 1600, false},
		{"CRLF 行尾", "Physical size: 1080x2400\r\nOverride size: 720x1600\r\n", 720, 1600, false},
		{"Override 后跟无关行", "Override size: 720x1600\nsome garbage here\n", 720, 1600, false},
		{"垃圾输出", "no size available\n", 0, 0, true},
		{"空输出", "", 0, 0, true},
		{"Physical 行数值非法但有 Override", "Physical size: abcxdef\nOverride size: 720x1600\n", 720, 1600, false},
		{"半截分辨率", "Physical size: 1080x\n", 0, 0, true},
		{"零分辨率非法", "Physical size: 0x0\n", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h, err := parseWmSize(tc.out)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseWmSize(%q) = (%d, %d, nil), 期望错误", tc.out, w, h)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWmSize(%q) 意外错误: %v", tc.out, err)
			}
			if w != tc.wantW || h != tc.wantH {
				t.Fatalf("parseWmSize(%q) = (%d, %d), want (%d, %d)", tc.out, w, h, tc.wantW, tc.wantH)
			}
		})
	}
}
