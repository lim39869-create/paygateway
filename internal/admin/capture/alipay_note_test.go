package capture

import (
	"os"
	"testing"
)

// TestParseAlipayDetailNote 详情页备注码提取（实测 fixture：付款留言标签后相邻节点文本即备注码）
func TestParseAlipayDetailNote(t *testing.T) {
	data, err := os.ReadFile("testdata/alipay_detail_note.xml")
	if err != nil {
		t.Fatalf("读取测试数据失败: %v", err)
	}
	if got := parseAlipayDetailNote(string(data)); got != "jzt4" {
		t.Fatalf("备注码应为 jzt4，实际 %q", got)
	}
}

// TestParseAlipayDetailNote_NoLabel 无付款留言标签（非详情页/无备注）→ 返回空
func TestParseAlipayDetailNote_NoLabel(t *testing.T) {
	if got := parseAlipayDetailNote(`<node text="收款详情"/>`); got != "" {
		t.Fatalf("无标签应返回空，实际 %q", got)
	}
}

// TestLedgerAmountBounds 账本行金额节点 bounds 提取（探测点击坐标）
func TestLedgerAmountBounds(t *testing.T) {
	data, err := os.ReadFile("testdata/alipay_ledger_ref.xml")
	if err != nil {
		t.Fatalf("读取测试数据失败: %v", err)
	}
	rows, err := ReadAlipayLedger(string(data))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("应至少 1 行")
	}
	for _, r := range rows {
		if r.AmountBounds == "" {
			t.Errorf("行 %s 缺少 AmountBounds（金额节点 bounds 应被填充）: %+v", r.Time, r)
		}
	}
}
