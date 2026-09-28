// 支付宝账本页(收款记录 XRiverTransActivity)服务端读取与解析。
// 背景：账本页是小程序容器（FLAG_SECURE），监听端无障碍 rootInActiveWindow 读不到节点树；
// 但 adb shell 的 uiautomator dump 是系统级通道，不受 SECURE 限制（实测可读出完整账本行）。
// 链路：adb uiautomator dump → XML → 解析账本行（付款方 | HH:MM:SS | +金额）。
// 账本行无备注（支付宝账本当前无备注行，Note 恒为空），去重指纹 = 时间|金额（秒级时间戳 + 金额）。
package capture

import (
	"encoding/xml"
	"fmt"
	"regexp"
	"strings"
)

// AlipayLedgerRow 账本页单行收款记录（去重指纹 = 时间|金额）
type AlipayLedgerRow struct {
	Payer        string // 付款方（脱敏，如 **怡 / 朱*6）
	Date         string // 所属日期分组（"今日/M月d日"原始文案，如 "昨日 8月24日" / "7月1日"）
	Time         string // HH:MM:SS
	Amount       string // 金额字符串（支付宝 +1.00 / 微信 ￥39.00）
	AmountMinor  int64  // 金额（分）
	Note         string // 付款方备注文本（微信账本"付款方备注：xxx"行；支付宝账本页无备注→空，需详情页探测）
	AmountBounds string // 金额节点 bounds "[l,t][r,b]"（探测详情页备注码的点击坐标；微信账本无需探测→空）
	onScreen     bool   // 是否屏幕可见行（离屏虚拟化行 bounds [0,0][0,0] 为 false；解析器内部用）
}

// Fingerprint 行去重指纹：时间|金额（秒级时间到秒，同金额多笔靠时间区分）
func (r AlipayLedgerRow) Fingerprint() string {
	return r.Time + "|" + r.Amount
}

// ReadAlipayLedger 通过 adb uiautomator dump 读取当前前台账本页，解析可见收款行。
// 前置条件：设备前台已在账本页（XRiverTransActivity）；不在则该次调用返回 0 行（调用方自行判断）。
// 离屏虚拟化行（bounds [0,0][0,0]）会被过滤，只返回当前屏幕可见行。
func ReadAlipayLedger(xmlText string) ([]AlipayLedgerRow, error) {
	root, err := parseUINodeTree(xmlText)
	if err != nil {
		return nil, fmt.Errorf("解析账本页 XML 失败: %w", err)
	}
	var rows []AlipayLedgerRow
	collectLedgerRows(root, &rows)
	return rows, nil
}

// uiNode uiautomator dump XML 的最小节点结构（只保留解析所需字段）
type uiNode struct {
	Text   string
	Desc   string
	Bounds string
	Kids   []*uiNode
}

// parseUINodeTree 用 encoding/xml 解码 uiautomator 层级树
func parseUINodeTree(xmlText string) (*uiNode, error) {
	dec := xml.NewDecoder(strings.NewReader(xmlText))
	var stack []*uiNode
	var root *uiNode
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &uiNode{}
			for _, a := range t.Attr {
				switch a.Name.Local {
				case "text":
					n.Text = a.Value
				case "content-desc":
					n.Desc = a.Value
				case "bounds":
					n.Bounds = a.Value
				}
			}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Kids = append(parent.Kids, n)
			} else {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
	if root == nil {
		return nil, fmt.Errorf("空 XML 层级树")
	}
	return root, nil
}

var (
	ledgerTimeRe   = regexp.MustCompile(`^\d{2}:\d{2}:\d{2}$`) // HH:MM:SS
	ledgerAmountRe = regexp.MustCompile(`^\+\d+(\.\d{2})?$`)   // +1.00 / +10
	zeroBoundsRe   = regexp.MustCompile(`\[0,0\]\[0,0\]`)
)

// collectLedgerRows 递归收集账本行：行 = 子树内恰好含 1 个时间 + 1 个金额文本的最小容器。
// 过滤：离屏虚拟化行（bounds [0,0][0,0]，Android 懒加载未渲染的真实节点）。
func collectLedgerRows(n *uiNode, rows *[]AlipayLedgerRow) {
	collectLedgerRowsGroup(n, "", rows)
}

// collectLedgerRowsGroup 带日期分组下钻：dateGroup 为当前所属日期分组文案（如 "今日 8月25日"）。
// 日期分组头（"今日/昨日/M月d日"）在账本中是独立文本节点，作为行收集时下钻到其子树继承。
func collectLedgerRowsGroup(n *uiNode, dateGroup string, rows *[]AlipayLedgerRow) {
	if n == nil {
		return
	}
	if dg, ok := ledgerDateGroup(n.Text); ok {
		dateGroup = dg
	}
	if timeText, amountText, isRow := ledgerRowTexts(n); isRow {
		if !zeroBoundsRe.MatchString(n.Bounds) {
			*rows = append(*rows, AlipayLedgerRow{
				Payer:        ledgerRowPayer(n, timeText, amountText),
				Date:         dateGroup,
				Time:         timeText,
				Amount:       amountText,
				AmountMinor:  ledgerAmountMinor(amountText),
				AmountBounds: ledgerAmountBounds(n),
			})
		}
		return // 行容器内不再深入（子节点无独立行）
	}
	for _, k := range n.Kids {
		collectLedgerRowsGroup(k, dateGroup, rows)
	}
}

// ledgerDateGroup 识别日期分组头文本（"今日 8月25日" / "昨日 8月24日" / "8月19日"）。
func ledgerDateGroup(text string) (string, bool) {
	if text == "" || len(text) > 16 {
		return "", false
	}
	if strings.HasPrefix(text, "今日 ") || strings.HasPrefix(text, "昨日 ") {
		return text, true
	}
	// 纯 "M月d日"（如 "8月19日"）
	if strings.Contains(text, "月") && strings.HasSuffix(text, "日") && !strings.ContainsAny(text, " 笔收额") {
		return text, true
	}
	return "", false
}

// ledgerRowTexts 判断节点是否为"账本行最小容器"：直系子树内含恰好 1 个时间 + 1 个金额文本。
// 返回 (时间文本, 金额文本, 是否行容器)。摘要区（收款笔数/总额 ¥2.00）无 HH:MM:SS 时间 → 不是行。
func ledgerRowTexts(n *uiNode) (string, string, bool) {
	var timeText, amountText string
	for _, k := range n.Kids {
		tt, at, ok := ledgerNodeTexts(k)
		if !ok {
			continue
		}
		if tt != "" {
			if timeText != "" {
				return "", "", false // 多时间 → 是分组容器，非单行
			}
			timeText = tt
		}
		if at != "" {
			if amountText != "" {
				return "", "", false
			}
			amountText = at
		}
	}
	if timeText != "" && amountText != "" {
		return timeText, amountText, true
	}
	return "", "", false
}

// ledgerNodeTexts 递归收集子树内的（首个）时间文本与金额文本。
func ledgerNodeTexts(n *uiNode) (timeText, amountText string, ok bool) {
	if n == nil {
		return "", "", false
	}
	if n.Text != "" {
		if ledgerTimeRe.MatchString(n.Text) {
			return n.Text, "", true
		}
		if ledgerAmountRe.MatchString(n.Text) {
			return "", n.Text, true
		}
	}
	for _, k := range n.Kids {
		tt, at, o := ledgerNodeTexts(k)
		if o {
			return tt, at, true
		}
	}
	return "", "", false
}

// ledgerAmountBounds 提取行容器内金额节点的 bounds（"[l,t][r,b]"）。
// 用途：支付宝账本页金额可点击进入收款详情页读备注码——探测点击坐标 = 金额 bounds 中心。
// 找不到返回 ""（调用方跳过探测）。
func ledgerAmountBounds(n *uiNode) string {
	if n == nil {
		return ""
	}
	if n.Text != "" && ledgerAmountRe.MatchString(n.Text) {
		return n.Bounds
	}
	for _, k := range n.Kids {
		if b := ledgerAmountBounds(k); b != "" {
			return b
		}
	}
	return ""
}

// ledgerRowPayer 提取行容器内付款方文本：非时间、非金额、非空白的文本（如 **怡）。
func ledgerRowPayer(n *uiNode, timeText, amountText string) string {
	var payer string
	var findPayer func(*uiNode)
	findPayer = func(m *uiNode) {
		if payer != "" {
			return
		}
		if m.Text != "" && m.Text != timeText && m.Text != amountText && !ledgerTimeRe.MatchString(m.Text) && !ledgerAmountRe.MatchString(m.Text) {
			payer = m.Text
			return
		}
		for _, k := range m.Kids {
			findPayer(k)
		}
	}
	findPayer(n)
	return payer
}

// ledgerAmountMinor 金额文本转分（"+1.00" → 100）
func ledgerAmountMinor(amount string) int64 {
	s := strings.TrimPrefix(amount, "+")
	var major, minor int64
	fmt.Sscanf(s, "%d.%d", &major, &minor)
	// "1.5" 补零成 "1.50"；"1" 无小数视为整元
	if i := strings.IndexByte(s, '.'); i >= 0 {
		frac := s[i+1:]
		switch len(frac) {
		case 1:
			minor *= 10
		case 2:
			// 保持原样
		default:
			minor = 0
		}
	} else {
		minor = 0
	}
	return major*100 + minor
}
