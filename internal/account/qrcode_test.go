package account

import "testing"

// TestInvalidQRContent_Wechat 微信收款码格式校验：wxp:// 与 weixin:// 合法，其它非法。
func TestInvalidQRContent_Wechat(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"wxp 合法", "wxp://f2f01PGBgX9q6lITQMxL7rZ14-C8jDWNkaOPZ7mw2cYhTYW8NiseyukoiyOI8nWRURWQ", false},
		{"weixin 合法", "weixin://wxpay/bizpayurl/up?pr=NwY5Mz9", false},
		{"空字符串非法", "", true},
		{"TESTMAIN 报错文案非法", "TESTMAIN-AGAIN", true},
		{"普通文本非法", "hello world", true},
		{"带换行非法", "wxp://abc\n", true},
		{"带空格非法", "wxp://ab c", true},
		{"带 tab 非法", "wxp://abc\t", true},
		{"https 微信码非法", "https://qr.wechat.com/xxx", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := InvalidQRContent("personal.wechat", tc.content)
			if tc.wantErr && reason == "" {
				t.Fatalf("期望非法（内容 %q），实际通过校验", tc.content)
			}
			if !tc.wantErr && reason != "" {
				t.Fatalf("期望合法（内容 %q），实际拒绝: %s", tc.content, reason)
			}
		})
	}
}

// TestInvalidQRContent_Alipay 支付宝收款码格式校验：https URL 合法，其它非法。
func TestInvalidQRContent_Alipay(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"qr.alipay 合法", "https://qr.alipay.com/fkx11063uortchy1ft90ibb", false},
		{"通用 https 合法", "https://example.com/pay", false},
		{"空字符串非法", "", true},
		{"报错文案非法", "SYSTEM_ERROR", true},
		{"http 非法", "http://qr.alipay.com/xxx", true},
		{"wxp 支付宝码非法", "wxp://f2f0xxx", true},
		{"带换行非法", "https://qr.alipay.com/xx\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason := InvalidQRContent("personal.alipay", tc.content)
			if tc.wantErr && reason == "" {
				t.Fatalf("期望非法（内容 %q），实际通过校验", tc.content)
			}
			if !tc.wantErr && reason != "" {
				t.Fatalf("期望合法（内容 %q），实际拒绝: %s", tc.content, reason)
			}
		})
	}
}

// TestInvalidQRContent_UnknownChannel 未知通道按 https URL 要求校验（保守默认）。
func TestInvalidQRContent_UnknownChannel(t *testing.T) {
	if reason := InvalidQRContent("merchant.wechat", "wxp://abc"); reason == "" {
		t.Fatal("非 personal 通道不应接受 wxp 码")
	}
	if reason := InvalidQRContent("", "https://x.com/pay"); reason != "" {
		t.Fatalf("空通道 https URL 应合法，实际拒绝: %s", reason)
	}
}

// TestSelectFromWeighted_PicksHighestScore 均衡分配：SelectFromWeighted 选评分最高账户
func TestSelectFromWeighted_PicksHighestScore(t *testing.T) {
	p := NewPool()
	p.Add(&Account{ID: 1, Channel: "personal.alipay", Group: "default", Name: "A", Priority: 1,
		DailyUsed: 900, DailyLimit: 1000, Status: StatusNormal})
	p.Add(&Account{ID: 2, Channel: "personal.alipay", Group: "default", Name: "B", Priority: 1,
		DailyUsed: 100, DailyLimit: 1000, Status: StatusNormal})
	p.Add(&Account{ID: 3, Channel: "personal.alipay", Group: "default", Name: "C", Priority: 1,
		DailyUsed: 0, DailyLimit: 1000, Status: StatusNormal})

	// 评分 = 日限额余量（1 - used/limit）：C(1.0) > B(0.9) > A(0.1)
	allowed := map[int64]bool{1: true, 2: true, 3: true}
	got := p.SelectFromWeighted("personal.alipay", "default", allowed,
		func(acct *Account) float64 {
			return 1 - float64(acct.DailyUsed)/float64(acct.DailyLimit)
		})
	if got == nil || got.ID != 3 {
		t.Fatalf("应选 C(3)，got %+v", got)
	}
}

// TestSelectFromWeighted_ExcludesDisallowed 均衡分配：allowedIDs 过滤生效
func TestSelectFromWeighted_ExcludesDisallowed(t *testing.T) {
	p := NewPool()
	p.Add(&Account{ID: 1, Channel: "personal.alipay", Group: "default", Name: "A", Priority: 1,
		DailyUsed: 900, DailyLimit: 1000, Status: StatusNormal})
	p.Add(&Account{ID: 2, Channel: "personal.alipay", Group: "default", Name: "B", Priority: 1,
		DailyUsed: 100, DailyLimit: 1000, Status: StatusNormal})

	// 只允许 1（评分低的 A）→ 必须选 A
	allowed := map[int64]bool{1: true}
	got := p.SelectFromWeighted("personal.alipay", "default", allowed,
		func(acct *Account) float64 {
			return 1 - float64(acct.DailyUsed)/float64(acct.DailyLimit)
		})
	if got == nil || got.ID != 1 {
		t.Fatalf("应选 A(1)（allowed 过滤），got %+v", got)
	}

	// 不允许任何 → nil
	got2 := p.SelectFromWeighted("personal.alipay", "default", map[int64]bool{999: true}, func(acct *Account) float64 { return 1 })
	if got2 != nil {
		t.Fatalf("无 allowed 账户应 nil，got %+v", got2)
	}
}

// TestSelectFromWeighted_ScoreTieBreakByID 评分相等 → ID 升序确定性
func TestSelectFromWeighted_ScoreTieBreakByID(t *testing.T) {
	p := NewPool()
	p.Add(&Account{ID: 5, Channel: "personal.alipay", Group: "default", Name: "E", Priority: 1,
		DailyUsed: 500, DailyLimit: 1000, Status: StatusNormal})
	p.Add(&Account{ID: 2, Channel: "personal.alipay", Group: "default", Name: "B", Priority: 1,
		DailyUsed: 500, DailyLimit: 1000, Status: StatusNormal})
	// 评分相同（0.5）→ 应选 ID 更小的 B(2)
	got := p.SelectFromWeighted("personal.alipay", "default", nil,
		func(acct *Account) float64 { return 0.5 })
	if got == nil || got.ID != 2 {
		t.Fatalf("评分相等应选 ID 升序的 B(2)，got %+v", got)
	}
}

// TestSelectFromWeighted_NilScoreFallsBack 评分函数为 nil → 退化为 SelectFrom（不 panic）
func TestSelectFromWeighted_NilScoreFallsBack(t *testing.T) {
	p := NewPool()
	p.Add(&Account{ID: 1, Channel: "personal.alipay", Group: "default", Name: "A", Priority: 1,
		DailyUsed: 500, DailyLimit: 1000, Status: StatusNormal})
	got := p.SelectFromWeighted("personal.alipay", "default", nil, nil)
	if got == nil || got.ID != 1 {
		t.Fatalf("nil score 应退化选取，got %+v", got)
	}
}
