package capture

import (
	"strings"
	"testing"
)

// 微信账本页真实 dump 片段（2026-08-29 真机红米K60，无障碍开启后 XWeb 暴露节点树）。
// 覆盖：多日期分组、金额 ￥ 前缀、付款方脱敏、备注行、离屏虚拟化行。
const wechatLedgerSampleXML = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
<node index="0" text="" class="android.widget.FrameLayout" bounds="[0,0][1080,2268]">
<node index="0" text="点单收款可得「提现免费额度」" class="android.widget.TextView" bounds="[157,275][750,341]"/>
<node index="0" text="自定义查询" class="android.widget.TextView" bounds="[199,464][425,530]"/>
<node index="0" text="7月1日" class="android.widget.TextView" bounds="[42,635][1039,703]"/>
<node index="0" text="共收款1笔，累计￥39.00" class="android.widget.TextView" bounds="[42,703][1039,758]"/>
<node index="0" text="朱*6" class="android.widget.TextView" bounds="[210,840][296,905]"/>
<node index="0" text="￥39.00" class="android.widget.TextView" bounds="[905,840][1039,905]"/>
<node index="0" text="17:24:47" class="android.widget.TextView" bounds="[210,921][354,979]"/>
<node index="0" text="6月3日" class="android.widget.TextView" bounds="[42,1078][1039,1144]"/>
<node index="0" text="共收款1笔，累计￥100.00" class="android.widget.TextView" bounds="[42,1147][1039,1202]"/>
<node index="0" text="*学" class="android.widget.TextView" bounds="[210,1283][273,1349]"/>
<node index="0" text="￥100.00" class="android.widget.TextView" bounds="[884,1283][1039,1349]"/>
<node index="0" text="15:36:32" class="android.widget.TextView" bounds="[210,1365][354,1420]"/>
<node index="0" text="9月14日" class="android.widget.TextView" bounds="[42,1522][1039,1588]"/>
<node index="0" text="共收款1笔，累计￥200.00" class="android.widget.TextView" bounds="[42,1590][1039,1648]"/>
<node index="0" text="*游" class="android.widget.TextView" bounds="[210,1727][273,1795]"/>
<node index="0" text="￥200.00" class="android.widget.TextView" bounds="[882,1727][1039,1795]"/>
<node index="0" text="16:13:03" class="android.widget.TextView" bounds="[210,1808][354,1866]"/>
<node index="0" text="付款方备注：电信这回" class="android.widget.TextView" bounds="[210,1871][1015,1926]"/>
<node index="0" text="仅展示近12个月收款记录，查看更多请" class="android.widget.TextView" bounds="[149,2241][782,2268]"/>
<node index="0" text="选择时间" class="android.widget.TextView" bounds="[779,2241][929,2268]"/>
</node>
</hierarchy>`

// 含离屏虚拟化行（bounds [0,0][0,0]）的片段：应被过滤
const wechatLedgerVirtualXML = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
<node index="0" text="" class="android.widget.FrameLayout" bounds="[0,0][1080,2268]">
<node index="0" text="9月14日" class="android.widget.TextView" bounds="[42,1522][1039,1588]"/>
<node index="0" text="*游" class="android.widget.TextView" bounds="[210,1727][273,1795]"/>
<node index="0" text="￥200.00" class="android.widget.TextView" bounds="[0,0][0,0]"/>
<node index="0" text="16:13:03" class="android.widget.TextView" bounds="[0,0][0,0]"/>
</node>
</hierarchy>`

func TestReadWechatLedger(t *testing.T) {
	rows, err := ReadWechatLedger(wechatLedgerSampleXML)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("应解析出 3 行，实际 %d 行: %+v", len(rows), rows)
	}

	// 第 1 行：7月1日 / 朱*6 / ￥39.00 / 17:24:47
	r0 := rows[0]
	if r0.Date != "7月1日" || r0.Payer != "朱*6" || r0.Amount != "￥39.00" || r0.Time != "17:24:47" {
		t.Errorf("第1行解析错误: %+v", r0)
	}
	if r0.AmountMinor != 3900 {
		t.Errorf("第1行金额应为 3900 分，实际 %d", r0.AmountMinor)
	}

	// 第 2 行：6月3日 / *学 / ￥100.00 / 15:36:32（金额无小数补零）
	r1 := rows[1]
	if r1.Date != "6月3日" || r1.Payer != "*学" || r1.Amount != "￥100.00" || r1.Time != "15:36:32" {
		t.Errorf("第2行解析错误: %+v", r1)
	}
	if r1.AmountMinor != 10000 {
		t.Errorf("第2行金额应为 10000 分，实际 %d", r1.AmountMinor)
	}

	// 第 3 行：9月14日 / *游 / ￥200.00 / 16:13:03
	r2 := rows[2]
	if r2.Date != "9月14日" || r2.Payer != "*游" || r2.Amount != "￥200.00" || r2.Time != "16:13:03" {
		t.Errorf("第3行解析错误: %+v", r2)
	}
	if r2.AmountMinor != 20000 {
		t.Errorf("第3行金额应为 20000 分，实际 %d", r2.AmountMinor)
	}

	// 备注行"付款方备注：电信这回"应捕获到 r2.Note，且不污染 Payer
	if r2.Note != "电信这回" {
		t.Errorf("备注应进入 r2.Note，实际 %q", r2.Note)
	}
	if strings.Contains(r2.Payer, "备注") {
		t.Errorf("备注文本不应进入付款方: %q", r2.Payer)
	}

	// 指纹（时间|金额）格式与支付宝一致
	if fp := r0.Fingerprint(); fp != "17:24:47|￥39.00" {
		t.Errorf("指纹格式错误: %q", fp)
	}
}

func TestReadWechatLedgerFiltersVirtualRows(t *testing.T) {
	rows, err := ReadWechatLedger(wechatLedgerVirtualXML)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("离屏虚拟化行应被过滤（金额/时间 bounds [0,0][0,0]），实际 %d 行: %+v", len(rows), rows)
	}
}

func TestReadWechatLedgerEmpty(t *testing.T) {
	// 无障碍未开启 → 微信 dump 黑盒（仅空 FrameLayout）→ 0 行
	rows, err := ReadWechatLedger(`<?xml version='1.0'?><hierarchy rotation="0"><node text="" class="android.widget.FrameLayout" bounds="[0,0][0,0]"/></hierarchy>`)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("空树应返回 0 行，实际 %d", len(rows))
	}
}

// TestReadWechatLedgerNoteBeforeAmount 备注行出现在金额之前：应作为 noteCandidate 暂存，
// 在下一个 pending 行（金额触发）创建时挂到该行的 Note 上。
func TestReadWechatLedgerNoteBeforeAmount(t *testing.T) {
	const xml = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
<node index="0" text="" class="android.widget.FrameLayout" bounds="[0,0][1080,2268]">
<node index="0" text="9月14日" class="android.widget.TextView" bounds="[42,1522][1039,1588]"/>
<node index="0" text="付款方备注：先备后付" class="android.widget.TextView" bounds="[210,1640][900,1695]"/>
<node index="0" text="*游" class="android.widget.TextView" bounds="[210,1727][273,1795]"/>
<node index="0" text="￥200.00" class="android.widget.TextView" bounds="[882,1727][1039,1795]"/>
<node index="0" text="16:13:03" class="android.widget.TextView" bounds="[210,1808][354,1866]"/>
</node>
</hierarchy>`
	rows, err := ReadWechatLedger(xml)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("期望 1 行，实际 %d 行: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Payer != "*游" || r.Amount != "￥200.00" || r.Time != "16:13:03" {
		t.Errorf("行基础字段错误: %+v", r)
	}
	if r.Note != "先备后付" {
		t.Errorf("备注应挂到下一行 Note，实际 %q", r.Note)
	}
}

// TestReadWechatLedgerTwoRemarksConsecutive 两笔相邻收款各自带备注，备注必须各自归位，
// 不能跨行漂移、不能合并、不能成为伪行。
func TestReadWechatLedgerTwoRemarksConsecutive(t *testing.T) {
	const xml = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
<node index="0" text="" class="android.widget.FrameLayout" bounds="[0,0][1080,2268]">
<node index="0" text="9月14日" class="android.widget.TextView" bounds="[42,1522][1039,1588]"/>
<node index="0" text="甲" class="android.widget.TextView" bounds="[210,1640][250,1700]"/>
<node index="0" text="￥10.00" class="android.widget.TextView" bounds="[900,1640][1039,1700]"/>
<node index="0" text="10:00:00" class="android.widget.TextView" bounds="[210,1722][354,1778]"/>
<node index="0" text="付款方备注：甲备注码" class="android.widget.TextView" bounds="[210,1780][900,1835]"/>
<node index="0" text="乙" class="android.widget.TextView" bounds="[210,1855][250,1915]"/>
<node index="0" text="￥20.00" class="android.widget.TextView" bounds="[900,1855][1039,1915]"/>
<node index="0" text="10:05:00" class="android.widget.TextView" bounds="[210,1937][354,1993]"/>
<node index="0" text="付款方备注：乙备注码" class="android.widget.TextView" bounds="[210,1995][900,2050]"/>
</node>
</hierarchy>`
	rows, err := ReadWechatLedger(xml)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("期望 2 行（备注不形成伪行），实际 %d 行: %+v", len(rows), rows)
	}
	if rows[0].Payer != "甲" || rows[0].Amount != "￥10.00" || rows[0].Time != "10:00:00" {
		t.Errorf("第 1 行基础字段错误: %+v", rows[0])
	}
	if rows[0].Note != "甲备注码" {
		t.Errorf("第 1 行 Note 应为 %q，实际 %q", "甲备注码", rows[0].Note)
	}
	if rows[1].Payer != "乙" || rows[1].Amount != "￥20.00" || rows[1].Time != "10:05:00" {
		t.Errorf("第 2 行基础字段错误: %+v", rows[1])
	}
	if rows[1].Note != "乙备注码" {
		t.Errorf("第 2 行 Note 应为 %q，实际 %q", "乙备注码", rows[1].Note)
	}
}

// TestReadWechatLedgerNoteDoesNotChangeRowCount 备注行不构成独立行：含 2 笔收款 + 3 条备注
// 的 dump 必须仍解析为 2 行。
func TestReadWechatLedgerNoteDoesNotChangeRowCount(t *testing.T) {
	const xml = `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0">
<node index="0" text="" class="android.widget.FrameLayout" bounds="[0,0][1080,2268]">
<node index="0" text="7月1日" class="android.widget.TextView" bounds="[42,635][1039,703]"/>
<node index="0" text="甲" class="android.widget.TextView" bounds="[210,750][250,810]"/>
<node index="0" text="￥10.00" class="android.widget.TextView" bounds="[900,750][1039,810]"/>
<node index="0" text="10:00:00" class="android.widget.TextView" bounds="[210,832][354,888]"/>
<node index="0" text="付款方备注：备注A" class="android.widget.TextView" bounds="[210,890][900,945]"/>
<node index="0" text="付款方备注：备注B" class="android.widget.TextView" bounds="[210,950][900,1005]"/>
<node index="0" text="付款方备注：备注C" class="android.widget.TextView" bounds="[210,1010][900,1065]"/>
<node index="0" text="乙" class="android.widget.TextView" bounds="[210,1080][250,1140]"/>
<node index="0" text="￥20.00" class="android.widget.TextView" bounds="[900,1080][1039,1140]"/>
<node index="0" text="10:05:00" class="android.widget.TextView" bounds="[210,1162][354,1218]"/>
</node>
</hierarchy>`
	rows, err := ReadWechatLedger(xml)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("期望 2 行（3 条备注均不构成伪行），实际 %d 行: %+v", len(rows), rows)
	}
	// 第 1 行：3 条备注全部挂到 pending 行，最后一条胜出
	if rows[0].Note != "备注C" {
		t.Errorf("第 1 行 Note 应为最后一条 %q，实际 %q", "备注C", rows[0].Note)
	}
	if rows[1].Note != "" {
		t.Errorf("第 2 行 Note 应为空（无备注行），实际 %q", rows[1].Note)
	}
}

// TestDegenerateDump 退化空 dump（无障碍未填充→只有根 FrameLayout）vs 真实渲染空页：
// 前者是'读取失败'不得建基线，后者（有标题/tab静态文本）可正常建空基线。
func TestDegenerateDump(t *testing.T) {
	degen := `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?><hierarchy rotation="0"><node index="0" text="" class="android.widget.FrameLayout" bounds="[0,0][1080,2268]"><node index="1" text="" class="android.view.View" bounds="[0,0][0,0]" /></node></hierarchy>`
	if !DegenerateDump(degen) {
		t.Errorf("退化空树应被判定为退化，实际返回 false")
	}
	// 真实渲染空页：有标题/页脚静态文本（即使 0 交易行）→ 非退化
	rendered := `<?xml version='1.0' encoding='UTF-8' standalone='yes' ?>
<hierarchy rotation="0"><node index="0" text="" class="android.widget.FrameLayout" bounds="[0,0][1080,2268]">
<node index="0" text="收款小账本" class="android.widget.TextView" bounds="[400,150][680,210]"/>
<node index="0" text="昨日 0 笔" class="android.widget.TextView" bounds="[60,600][500,660]"/>
<node index="0" text="仅展示近12个月收款记录" class="android.widget.TextView" bounds="[149,2241][782,2268]"/>
</node></hierarchy>`
	if DegenerateDump(rendered) {
		t.Errorf("真实渲染空页（有标题/页脚文本）不应被判为退化，实际返回 true")
	}
	// 正常有交易行的 dump 显然非退化
	if DegenerateDump(wechatLedgerSampleXML) {
		t.Errorf("正常含交易行的 dump 不应被判为退化")
	}
}
