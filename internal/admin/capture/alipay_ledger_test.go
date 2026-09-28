package capture

import (
	"os"
	"testing"
)

func TestReadAlipayLedger(t *testing.T) {
	data, err := os.ReadFile("testdata/alipay_ledger_ref.xml")
	if err != nil {
		t.Fatalf("读取测试数据失败: %v", err)
	}
	rows, err := ReadAlipayLedger(string(data))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 参考 dump 可见行：昨日(8月24日) 2 笔（18:18:38 +1.00、17:16:02 +1.00）；
	// 8月19日 离屏行 bounds=[0,0][0,0] 应被过滤。
	if len(rows) != 2 {
		t.Fatalf("期望 2 行可见账本行（离屏行被过滤），实际 %d 行: %+v", len(rows), rows)
	}
	got := map[string]AlipayLedgerRow{}
	for _, r := range rows {
		got[r.Time] = r
	}
	if r, ok := got["18:18:38"]; !ok {
		t.Errorf("缺少 18:18:38 行: %+v", rows)
	} else if r.Amount != "+1.00" || r.AmountMinor != 100 {
		t.Errorf("18:18:38 行金额错误: %+v", r)
	} else if r.Payer != "**怡" {
		t.Errorf("18:18:38 行付款方错误: %+v", r)
	}
	if r, ok := got["17:16:02"]; !ok {
		t.Errorf("缺少 17:16:02 行: %+v", rows)
	} else if r.Amount != "+1.00" || r.AmountMinor != 100 {
		t.Errorf("17:16:02 行金额错误: %+v", r)
	}
}

func TestLedgerAmountMinor(t *testing.T) {
	cases := map[string]int64{
		"+1.00": 100,
		"+1.50": 150,
		"+10":   1000,
		"+0.01": 1,
	}
	for in, want := range cases {
		if got := ledgerAmountMinor(in); got != want {
			t.Errorf("ledgerAmountMinor(%q) = %d, 期望 %d", in, got, want)
		}
	}
}

func TestReadAlipayLedgerInvalidXML(t *testing.T) {
	if _, err := ReadAlipayLedger("not xml"); err == nil {
		t.Error("无效 XML 应返回错误")
	}
}
