// 支付宝收款详情页备注码探测（2026-08-29 真机实测）。
//
// 背景：支付宝账本页（XRiverActivity$App01）只显示 付款方|时间|金额，不显示备注码；
// 备注码（4 位字母数字）只存在于单笔收款详情页，需点击该行金额进入详情页读取。
//
// 实测链路（红米K60 / 主空间支付宝，2026-08-29）：
//
//	账本页金额节点 bounds 中心点击 → 详情页（收款详情）→ dump → "付款留言" 标签后
//	相邻节点文本即备注码（如 jzt4 / 7x57 / 7u6g）→ KEYCODE_BACK 返回账本页。
//
// 耗时实测：点击 0.17s + 详情加载 1.0s + dump 2.36s + 返回 0.25s ≈ 单笔 3.5s。
// 优化：金额唯一时跳过探测（调用方短路）；详情加载等待取 0.8s（布局静态，实测 0.5s 已渲染）。
package capture

import (
	"context"
	"fmt"
	"strings"
	"time"

	"paygateway/internal/adbmgr"
)

// alipayDetailNoteLabel 详情页备注码标签文案（实测：收款详情页"付款留言"行）
const alipayDetailNoteLabel = "付款留言"

// alipayDetailLoadWait 点击金额后详情页渲染等待（实测 0.5s 已就绪，留 0.8s 余量）
const alipayDetailLoadWait = 800 * time.Millisecond

// ProbeAlipayNote 从支付宝账本行探测备注码：点金额 → 详情页 dump → 提取备注 → 返回账本页。
// bounds 为账本行金额节点 bounds（"[l,t][r,b]"），点击其中心进入收款详情页。
// 返回备注码（可能为空：详情页无"付款留言"标签）；失败返回错误（调用方跳过该行探测，
// 金额兜底匹配仍可用——探测只是加速精确定位，不是资金安全前置）。
func ProbeAlipayNote(ctx context.Context, adb *adbmgr.Manager, serial, bounds string) (string, error) {
	if bounds == "" {
		return "", fmt.Errorf("金额 bounds 为空，无法探测备注码")
	}
	cx, cy, err := parseBoundsCenter(bounds)
	if err != nil {
		return "", fmt.Errorf("解析金额 bounds 失败: %w", err)
	}
	// 1. 点击金额进入收款详情页
	if err := adb.Tap(ctx, serial, cx, cy); err != nil {
		return "", fmt.Errorf("点击金额进入详情失败: %w", err)
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(alipayDetailLoadWait):
	}
	// 2. dump 详情页并解析备注码
	xml, err := adb.DumpUI(ctx, serial)
	if err != nil {
		return "", fmt.Errorf("详情页 dump 失败: %w", err)
	}
	note := parseAlipayDetailNote(xml)
	// 3. 返回账本页（不等动画，下轮 EnsureForeground 恢复）
	_ = adb.KeyEvent(ctx, serial, 4) // KEYCODE_BACK
	return note, nil
}

// parseAlipayDetailNote 从收款详情页 dump XML 提取备注码。
// 规则（实测结构）：找 "付款留言" 标签节点，其后紧跟的兄弟节点 text 即备注码。
// 返回 "" 表示详情页无付款留言（未命中备注码——调用方降级金额兜底）。
func parseAlipayDetailNote(xmlText string) string {
	root, err := parseUINodeTree(xmlText)
	if err != nil {
		return ""
	}
	// 扁平收集带文本节点（保序），找标签后下一个非空文本
	var nodes []textNode
	collectFlatTexts(root, &nodes)
	for i, n := range nodes {
		if strings.TrimSpace(n.text) == alipayDetailNoteLabel {
			// 备注码 = 标签后首个非空文本节点（实测 index 6→7 相邻）
			for j := i + 1; j < len(nodes); j++ {
				v := strings.TrimSpace(nodes[j].text)
				if v != "" {
					return v
				}
			}
		}
	}
	return ""
}
