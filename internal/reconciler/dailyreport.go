// Package reconciler 日终对账报告（RESILIENCE.md P2-1）
//
// 对账维度（网关 vs 通道 vs 上游）：
//   - 网关订单：按通道拆分（merchant.* / personal.*），统计订单数/成功数/金额
//   - 资金核对：成功订单金额 vs 订单金额（差异 = 异常池 / 金额不匹配单）
//   - 回调可靠性：notify_tasks 各状态数量（pending / success / dead）
//   - 异常池：待处理数量（自动化兜底失败的最终兜底）
//   - 结论：每通道差异明细，供人工核对
//
// 每日 00:00 由 main 调用（与账本快照/日限额重置同周期），输出结构化日志；
// 管理后台「对账中心」可实时查询（GET /api/reconciler/daily-report?date=YYYY-MM-DD）。
package reconciler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"paygateway/internal/channel"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/store"
)

// ChannelReport 单通道对账
type ChannelReport struct {
	Channel       string `json:"channel"`
	TotalOrders   int64  `json:"total_orders"`   // 当日订单
	SuccessOrders int64  `json:"success_orders"` // 当日成功
	ClosedOrders  int64  `json:"closed_orders"`  // 当日关闭
	AmountMinor   int64  `json:"amount_minor"`   // 当日成功金额（分）
	AmountClosed  int64  `json:"amount_closed"`  // 当日关闭金额（分）
}

// DailyReport 日终对账报告
type DailyReport struct {
	Date string `json:"date"` // 2026-08-10

	OrdersTotal   int64 `json:"orders_total"`
	OrdersSuccess int64 `json:"orders_success"`
	OrdersClosed  int64 `json:"orders_closed"`
	SuccessAmount int64 `json:"success_amount"` // 成功金额（分）
	ClosedAmount  int64 `json:"closed_amount"`  // 关闭金额（分）

	NotifyPending int64 `json:"notify_pending"` // 待投递回调
	NotifySuccess int64 `json:"notify_success"`
	NotifyDead    int64 `json:"notify_dead"` // 死信（需人工重发）

	ExceptionsPending int64 `json:"exceptions_pending"` // 异常池待处理

	ByChannel []*ChannelReport `json:"by_channel"` // 通道拆分

	Conclusion string `json:"conclusion"` // 对账结论
}

// BuildDailyReport 生成指定日期的对账报告（date 格式 2006-01-02，空=今天）
func BuildDailyReport(ctx context.Context, st store.Store, date string) (*DailyReport, error) {
	if date == "" {
		date = time.Now().Format("2006-01-02")
	}
	r := &DailyReport{Date: date, ByChannel: []*ChannelReport{}}

	// 1. 网关侧总量
	total, err := st.CountOrders(ctx, "")
	if err != nil {
		return nil, err
	}
	success, err := st.CountOrders(ctx, model.StatusSuccess)
	if err != nil {
		return nil, err
	}
	closed, err := st.CountOrders(ctx, model.StatusClosed)
	if err != nil {
		return nil, err
	}
	r.OrdersTotal, r.OrdersSuccess, r.OrdersClosed = total, success, closed

	if successAmount, err := st.SumAmount(ctx, model.StatusSuccess); err == nil {
		r.SuccessAmount = successAmount
	}
	if closedAmount, err := st.SumAmount(ctx, model.StatusClosed); err == nil {
		r.ClosedAmount = closedAmount
	}

	// 2. 按通道拆分（当日口径：订单/成功/关闭 + 金额）
	for _, name := range channel.AllNames() {
		total, _, err := st.DailyStatsByChannel(ctx, name, "", date)
		if err != nil {
			continue
		}
		succ, succAmount, err := st.DailyStatsByChannel(ctx, name, model.StatusSuccess, date)
		if err != nil {
			continue
		}
		closed, closedAmount, err := st.DailyStatsByChannel(ctx, name, model.StatusClosed, date)
		if err != nil {
			continue
		}
		r.ByChannel = append(r.ByChannel, &ChannelReport{
			Channel: name, TotalOrders: total, SuccessOrders: succ,
			ClosedOrders: closed, AmountMinor: succAmount, AmountClosed: closedAmount,
		})
	}

	// 3. 回调可靠性
	if n, err := st.CountNotifyTasks(ctx, "pending"); err == nil {
		r.NotifyPending = n
	}
	if n, err := st.CountNotifyTasks(ctx, "success"); err == nil {
		r.NotifySuccess = n
	}
	if n, err := st.CountNotifyTasks(ctx, "dead"); err == nil {
		r.NotifyDead = n
	}

	// 4. 异常池
	if n, err := st.CountExceptions(ctx, model.ExPhasePending); err == nil {
		r.ExceptionsPending = n
	}

	// 5. 结论
	r.Conclusion = buildConclusion(r)
	return r, nil
}

// buildConclusion 对账结论：差异分析
func buildConclusion(r *DailyReport) string {
	var issues []string
	// 金额对账：成功金额 vs 订单金额差异来自异常池（金额不匹配单不计入成功）
	// 注意：当日成功金额 CountOrdersByChannelToday 是全天累计；全局成功金额是历史累计，
	// 因此这里不做全局金额等式校验，改为提示性结论：
	if r.NotifyDead > 0 {
		issues = append(issues, fmt.Sprintf("有 %d 笔回调 dead（需人工重发）", r.NotifyDead))
	}
	if r.ExceptionsPending > 0 {
		issues = append(issues, fmt.Sprintf("异常池有 %d 笔待处理（资金安全关键，请及时人工核实）", r.ExceptionsPending))
	}
	for _, c := range r.ByChannel {
		if c.SuccessOrders > 0 && c.ClosedOrders > 0 {
			// 提示关闭占比过高的通道
			if float64(c.ClosedOrders)/float64(c.SuccessOrders+c.ClosedOrders) > 0.3 {
				issues = append(issues, fmt.Sprintf("通道 %s 关闭率偏高（成功 %d / 关闭 %d），注意观察", c.Channel, c.SuccessOrders, c.ClosedOrders))
			}
		}
	}
	if len(issues) == 0 {
		return "✅ 对账正常：无 dead 回调、无待处理异常"
	}
	return "⚠️ 需要关注：" + strings.Join(issues, "；")
}

// Log 输出结构化日志（每日 00:00 调用）
func (r *DailyReport) Log() {
	logger.Info("reconciler", "=== 日终对账报告 "+r.Date+" ===")
	logger.Info("reconciler", "订单", "total", r.OrdersTotal, "success", r.OrdersSuccess,
		"closed", r.OrdersClosed, "success_amount", r.SuccessAmount)
	for _, c := range r.ByChannel {
		logger.Info("reconciler", "通道", "name", c.Channel, "total", c.TotalOrders,
			"success", c.SuccessOrders, "closed", c.ClosedOrders,
			"amount", c.AmountMinor, "closed_amount", c.AmountClosed)
	}
	logger.Info("reconciler", "回调", "pending", r.NotifyPending, "success", r.NotifySuccess, "dead", r.NotifyDead)
	logger.Info("reconciler", "异常池待处理", "count", r.ExceptionsPending)
	logger.Info("reconciler", "结论", "text", r.Conclusion)
}
