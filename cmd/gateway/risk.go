// paygateway 无订单收款行为标记（2026-09-14 需求确认）：
// 直发码无法阻止他人转账，熔断无意义且会误伤正常收款——
// 方向改为【准确记录 + 行为标记 + 告警提示立即核实】，保留司法可核对记录。
// 打分只做信息标注（risk_level/risk_tags），绝不影响入账与订单链路。
package main

import (
	"context"
	"fmt"
	"paygateway/internal/logger"
	"paygateway/internal/personal"
	"strings"
	"time"
)

// 风险等级
const (
	riskNone        = 0
	riskSuspect     = 1 // 建议立即核实
	riskHighWarning = 2 // 多重特征叠加，高警戒
)

const riskWindow = 30 * time.Minute

// assessUnmatched 行为模式标记：对一笔无订单收款计算风险标签。
// 标签（保守，宁可少标不误标）：
//
//	big_amount   单笔 ≥ PersonalBigAmount（默认 200 元，admin 可调）
//	burst        同设备窗口内无订单收款 ≥5 笔（批量二维码套现特征）
//	repeat_payer 同付款人窗口内重复出现 ≥2 笔（同源资金试探特征）
func (a *App) assessUnmatched(ctx context.Context, provider string, amountMinor int64, payer, deviceID string) (tags []string, level int) {
	if cfg := a.cfg; cfg != nil && cfg.PersonalBigAmount > 0 && amountMinor >= cfg.PersonalBigAmount {
		tags = append(tags, "big_amount")
	}
	recent, err := a.personalDB.ListUnmatched(ctx, "", 200)
	if err == nil {
		tags = append(tags, scoreFromRows(amountMinor, payer, deviceID, recent, time.Now(), riskWindow)...)
	}
	level = riskNone
	switch {
	case len(tags) >= 2:
		level = riskHighWarning
	case len(tags) == 1 && tags[0] == "big_amount":
		level = riskHighWarning
	case len(tags) == 1:
		level = riskSuspect
	}
	return tags, level
}

// scoreFromRows 纯计数逻辑（便于单测）：按窗口过滤历史行并产出 burst/repeat_payer 标签
func scoreFromRows(amountMinor int64, payer, deviceID string, rows []*personal.UnmatchedPayment, now time.Time, window time.Duration) (tags []string) {
	burstMax, repeatMax := 0, 0
	for _, u := range rows {
		if u.CreatedAt.IsZero() || now.Sub(u.CreatedAt) > window {
			continue
		}
		if u.DeviceID != "" && u.DeviceID == deviceID {
			burstMax++
		}
		if payer != "" && u.Payer != "" && payer == u.Payer {
			repeatMax++
		}
	}
	if burstMax+1 >= 5 {
		tags = append(tags, "burst")
	}
	if repeatMax+1 >= 2 {
		tags = append(tags, "repeat_payer")
	}
	return tags
}

// saveUnmatched 记录无订单收款（富化 + 行为标记 + 告警提示）。
func (a *App) saveUnmatched(ctx context.Context, provider string, amountMinor int64, note, raw, payer, deviceID string) {
	tags, level := a.assessUnmatched(ctx, provider, amountMinor, payer, deviceID)
	if _, err := a.personalDB.CreateUnmatched(ctx, &personal.UnmatchedPayment{
		Provider: provider, AmountMinor: amountMinor, Note: note, Raw: raw,
		Status: "pending", Payer: payer, DeviceID: deviceID,
		RiskTags: strings.Join(tags, ","), RiskLevel: level,
	}); err != nil {
		logger.Error("risk", "无订单收款落库失败", "err", err, "amount", amountMinor)
		return
	}
	if level <= riskNone {
		logger.Info("risk", "无订单收款已记录（无风险特征）", "provider", provider, "amount", amountMinor, "device", deviceID)
		return
	}
	logger.Warn("risk", "无订单收款行为标记，建议立即核实",
		"risk_tags", strings.Join(tags, ","), "risk_level", level,
		"provider", provider, "amount", amountMinor, "payer", payer, "device", deviceID)
	if a.alertC != nil {
		a.alertC.Send("error", "疑似无订单收款（黑钱风险提示）",
			fmt.Sprintf("来源: %s\n金额: ¥%.2f\n付款人: %s\n收款设备: %s\n特征: %s\n\n疑似黑钱/赃款风险，建议立即核实该笔收款；确认异常的请保全记录并联系司法部门处理。",
				provider, float64(amountMinor)/100, payerOrDash(payer), deviceID, strings.Join(tags, ",")))
	}
}

func payerOrDash(p string) string {
	if p == "" {
		return "（通知栏无付款人信息）"
	}
	return p
}
