// 微信收款记录页（AppBrandUI00 小程序容器）服务端读取与解析。
// 背景：微信对无障碍严格屏蔽，uiautomator dump 默认黑盒（仅 1 个空 FrameLayout 节点）。
// 解锁：系统无障碍开启（settings put secure accessibility_enabled 1）后 XWeb 小程序
// 才构建/暴露 accessibility 节点树（docs/DESIGN-DEVICE-ACTOR.md §9.1 实测确认）。
// 链路：adb uiautomator dump → XML → 解析账本行（付款方 | ￥金额 | HH:MM:SS）。
// 与支付宝差异：微信行是扁平文本节点（无嵌套容器），金额带 ￥ 前缀，日期无"今日/昨日"。
package capture

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// 微信账本行正则（实测 2026-08-29 真机）：
//   - 金额：￥39.00 / ￥100（带 ￥ 前缀）
//   - 时间：17:24:47（HH:MM:SS，与支付宝同格式）
//   - 日期分组：7月1日 / 9月14日（M月d日，无"今日/昨日"前缀）
var (
	wechatAmountRe = regexp.MustCompile(`^￥\d+(\.\d{2})?$`) // ￥39.00 / ￥100
	wechatDateRe   = regexp.MustCompile(`^\d{1,2}月\d{1,2}日$`)
	// 付款方备注行（携带"备注码"，是入账对账的反错配主信号）
	wechatNoteRe = regexp.MustCompile(`^付款方备注：(.+)$`)
	// 非付款方的干扰文本（页脚/摘要/入口等，付款方是短脱敏名不应含这些词）
	wechatNonPayerRe = regexp.MustCompile(`点单收款|自定义查询|共收款|累计|仅展示|选择时间|查看更多`)
)

// ReadWechatLedger 通过 adb uiautomator dump 读取当前前台微信收款记录页，解析可见收款行。
// 前置条件：系统无障碍已开启（accessibility_enabled=1）+ 前台在账本页（AppBrandUI00）。
// 行结构（扁平节点，同 y 带三要素）：
//
//	[210,y]   付款方（脱敏，如 朱*6 / *学）
//	[~900,y]  金额（￥39.00）
//	[210,y+81] 时间（17:24:47）
//
// 日期分组独立成行（M月d日），作为后续行的 Date 继承；
// 备注行（付款方备注：xxx）捕获到当前/下一行的 Note 字段，是入账对账的反错配主信号。
func ReadWechatLedger(xmlText string) ([]AlipayLedgerRow, error) {
	root, err := parseUINodeTree(xmlText)
	if err != nil {
		return nil, fmt.Errorf("解析微信账本页 XML 失败: %w", err)
	}
	// 扁平收集所有带文本的节点（微信账本行是扁平结构，无嵌套容器）
	var nodes []textNode
	collectFlatTexts(root, &nodes)

	var rows []AlipayLedgerRow
	dateGroup := ""
	// dump 输出顺序即屏幕从上到下（xml 深度优先），顺序扫描：
	// 行 = 金额节点（触发新行）→ 补时间 → 补付款方；离屏行（bounds [0,0][0,0]）过滤。
	// 付款方在金额之前出现（[210,y] vs [900,y]），但此时 pending 未创建，
	// 所以用"最近未消费的候选付款方"机制：非金额/时间/日期/干扰词的短文本先暂存，
	// 遇到金额时若暂存候选存在且 y 相近则归属该行。
	var payerCandidate string // 金额节点前最近的付款方候选
	var pending *AlipayLedgerRow
	var noteCandidate string // 备注文本候选（出现在金额之前则暂存，归属下一个 pending 行）
	for _, n := range nodes {
		switch {
		case wechatDateRe.MatchString(n.text):
			// 日期分组行：更新当前日期上下文，结束未完成行，清候选
			flushWechatRow(&rows, &pending, payerCandidate)
			dateGroup = n.text
			payerCandidate = ""
		case wechatAmountRe.MatchString(n.text):
			// 金额行：新行起点，归属最近的付款方候选（若有）
			flushWechatRow(&rows, &pending, payerCandidate)
			pending = &AlipayLedgerRow{
				Date: dateGroup, Payer: payerCandidate, Amount: n.text,
				AmountMinor: wechatAmountMinor(n.text), onScreen: !zeroBoundsRe.MatchString(n.bounds),
			}
			if noteCandidate != "" {
				pending.Note = noteCandidate
				noteCandidate = ""
			}
			payerCandidate = ""
		case ledgerTimeRe.MatchString(n.text) && pending != nil && pending.Time == "":
			// 时间行：归属当前待完成行
			pending.Time = n.text
		case wechatNoteRe.MatchString(n.text):
			// 备注行：归属当前 pending（若有），否则作为候选归属下一个 pending 行
			m := wechatNoteRe.FindStringSubmatch(n.text)
			note := m[1]
			if pending != nil {
				pending.Note = note
				noteCandidate = ""
			} else {
				noteCandidate = note
			}
		case wechatPayerText(n.text):
			// 付款方候选（非金额/时间/日期/干扰词的短文本）
			payerCandidate = n.text
		}
	}
	flushWechatRow(&rows, &pending, payerCandidate)
	return rows, nil
}

// wechatPayerText 判断文本是否为付款方候选（短脱敏名，非金额/时间/日期/干扰词）
func wechatPayerText(text string) bool {
	if text == "" || len([]rune(text)) > 8 {
		return false
	}
	if wechatDateRe.MatchString(text) || ledgerTimeRe.MatchString(text) || wechatAmountRe.MatchString(text) {
		return false
	}
	if wechatNonPayerRe.MatchString(text) {
		return false
	}
	return true
}

// textNode 带 bounds 的扁平文本节点
type textNode struct {
	text   string
	bounds string
}

// collectFlatTexts 扁平收集所有带文本的节点（按 dump 顺序即屏幕从上到下）。
func collectFlatTexts(n *uiNode, out *[]textNode) {
	if n == nil {
		return
	}
	if n.Text != "" {
		*out = append(*out, textNode{n.Text, n.Bounds})
	}
	for _, k := range n.Kids {
		collectFlatTexts(k, out)
	}
}

// flushWechatRow 结束当前待完成行并加入结果（缺时间/金额/离屏的行丢弃）
func flushWechatRow(rows *[]AlipayLedgerRow, pending **AlipayLedgerRow, payerCandidate string) {
	p := *pending
	if p == nil {
		return
	}
	if p.Payer == "" {
		p.Payer = payerCandidate
	}
	if p.Time != "" && p.Amount != "" && p.onScreen {
		*rows = append(*rows, *p)
	}
	*pending = nil
}

// wechatAmountMinor 微信金额文本转分（"￥39.00" → 3900；"￥100" → 10000）
func wechatAmountMinor(amount string) int64 {
	s := strings.TrimPrefix(amount, "￥")
	if i := strings.IndexByte(s, '.'); i >= 0 {
		major, _ := strconv.ParseInt(s[:i], 10, 64)
		frac := s[i+1:]
		minor := int64(0)
		switch len(frac) {
		case 1:
			minor, _ = strconv.ParseInt(frac+"0", 10, 64)
		case 2:
			minor, _ = strconv.ParseInt(frac, 10, 64)
		}
		return major*100 + minor
	}
	major, _ := strconv.ParseInt(s, 10, 64)
	return major * 100
}
