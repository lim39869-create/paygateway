package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"paygateway/internal/logger"
	"paygateway/internal/model"
	"paygateway/internal/order"
	"paygateway/internal/reconciler"
	"paygateway/internal/store"
)

// ReconcilerHandler 容灾对账中心：人工异常池 + dead 通知管理
type ReconcilerHandler struct {
	store store.Store
	svc   *order.Service
	sched *reconciler.Scheduler
}

// NewReconcilerHandler 创建
func NewReconcilerHandler(st store.Store, svc *order.Service, sched *reconciler.Scheduler) *ReconcilerHandler {
	return &ReconcilerHandler{store: st, svc: svc, sched: sched}
}

// RegisterRoutes 注册路由（需在 authMiddleware 组内）
func (h *ReconcilerHandler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/reconciler/exceptions", h.handleListExceptions)
	rg.GET("/reconciler/exceptions/:id", h.handleGetException)
	rg.POST("/reconciler/exceptions/:id/requery", h.handleRequery)
	rg.POST("/reconciler/exceptions/:id/resolve", h.handleResolve)
	rg.POST("/reconciler/exceptions/:id/reopen", h.handleReopen)
	rg.POST("/reconciler/exceptions", h.handleManualAdd) // 手动加入异常池
	rg.GET("/reconciler/notify-dead", h.handleListDead)
	rg.POST("/reconciler/notify-dead/:id/retry", h.handleRetryDead)
	rg.POST("/reconciler/notify-dead/:id/delete", h.handleDeleteDead)
	// 日终对账报告（RESILIENCE P2-1）：GET /reconciler/daily-report?date=YYYY-MM-DD
	rg.GET("/reconciler/daily-report", h.handleDailyReport)
}

// ========== 异常池 ==========

func (h *ReconcilerHandler) handleListExceptions(c *gin.Context) {
	phase := c.Query("phase")
	reason := c.Query("reason")
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	limit, offset := pageSize, (page-1)*pageSize

	list, err := h.store.ListExceptions(c.Request.Context(), phase, reason, limit, offset)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	total, err := h.store.CountExceptions(c.Request.Context(), phase)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"list": list, "total": total, "page": page}})
}

func (h *ReconcilerHandler) handleGetException(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	e, err := h.store.GetException(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "异常单不存在"})
		return
	}
	logs, err := h.store.ListExceptionLogs(c.Request.Context(), id, 100)
	if err != nil {
		logs = nil
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"exception": e, "logs": logs}})
}

// handleRequery 再次查单，刷新证据（证据自动收集，人工不可编辑）
func (h *ReconcilerHandler) handleRequery(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	e, err := h.store.GetException(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "异常单不存在"})
		return
	}
	st, qerr := h.sched.Query(c.Request.Context(), e.OutTradeNo)
	now := time.Now().Format(time.RFC3339)
	evidence := map[string]any{"action": "manual_requery", "time": now}
	if qerr != nil {
		evidence["error"] = qerr.Error()
		evidence["channel_status"] = "QUERY_FAILED"
	} else {
		evidence["channel_status"] = st.Status
		evidence["channel_tx_id"] = st.ChannelTxID
		evidence["amount_minor"] = st.AmountMinor
	}
	evJSON, _ := json.Marshal(evidence)

	// 追加证据（存到 evidence，简单拼接时间线）
	refreshedStatus := ""
	if qerr != nil {
		refreshedStatus = "QUERY_FAILED"
	} else {
		refreshedStatus = st.Status
	}
	_ = h.store.UpdateException(c.Request.Context(), id, func(up *model.ExceptionOrder) {
		up.ChannelStatus = refreshedStatus
		if qerr == nil && st.ChannelTxID != "" {
			up.ChannelTxID = st.ChannelTxID
		}
		if up.Evidence != "" {
			up.Evidence = up.Evidence + "\n" + string(evJSON)
		} else {
			up.Evidence = string(evJSON)
		}
	})
	operator := c.GetString("username")
	_ = h.store.InsertExceptionLog(c.Request.Context(), id, "requery", operator, string(evJSON))
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"channel_status": refreshedStatus, "evidence": evJSON}})
}

// handleResolve 人工处理：settle / close / noop
func (h *ReconcilerHandler) handleResolve(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var req struct {
		Action      string `json:"action"`
		PaidAmount  int64  `json:"paid_amount"`
		ChannelTxID string `json:"channel_tx_id"`
		Payer       string `json:"payer"`
		Note        string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: " + err.Error()})
		return
	}
	if req.Action != model.ExActionSettle && req.Action != model.ExActionClose && req.Action != model.ExActionNoop {
		c.JSON(http.StatusBadRequest, gin.H{"error": "动作必须是 settle/close/noop"})
		return
	}
	operator := c.GetString("username")
	ctx := c.Request.Context()

	e, err := h.store.GetException(ctx, id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "异常单不存在"})
		return
	}
	if e.Phase == model.ExPhaseResolved {
		c.JSON(http.StatusBadRequest, gin.H{"error": "该异常单已处理"})
		return
	}

	// 并发防重：pending -> processing 条件更新（抢占锁）
	locked, err := h.store.UpdateExceptionPhase(ctx, id, model.ExPhasePending, model.ExPhaseProcessing)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "加锁失败: " + err.Error()})
		return
	}
	if !locked {
		// 锁未抢到：说明存在并发处理（另一个请求已把 pending → processing）
		c.JSON(http.StatusConflict, gin.H{"error": "该异常单正在被处理或已被抢占，请刷新后重试"})
		return
	}
	if req.Action == model.ExActionSettle && req.PaidAmount < 0 {
		_ = h.store.UpdateException(ctx, id, func(up *model.ExceptionOrder) { up.Phase = model.ExPhasePending })
		c.JSON(http.StatusBadRequest, gin.H{"error": "金额不能为负"})
		return
	}

	var result string
	switch req.Action {
	case model.ExActionSettle:
		ok, serr := h.svc.SettleManual(ctx, e.OutTradeNo, req.ChannelTxID, req.PaidAmount, req.Payer, operator, req.Note)
		if serr != nil {
			_ = h.store.UpdateException(ctx, id, func(up *model.ExceptionOrder) { up.Phase = model.ExPhasePending })
			c.JSON(http.StatusInternalServerError, gin.H{"error": "入账失败: " + serr.Error()})
			return
		}
		if !ok {
			_ = h.store.UpdateException(ctx, id, func(up *model.ExceptionOrder) { up.Phase = model.ExPhasePending })
			c.JSON(http.StatusConflict, gin.H{"error": "订单状态不允许入账（可能已被处理）"})
			return
		}
		// 订单人工处理标记
		_ = h.store.UpdateOrderHandled(ctx, e.OutTradeNo, id, "manual_settle", operator, req.Note)
		result = "已确认入账，并通知上游"
	case model.ExActionClose:
		ok, cerr := h.svc.CloseOrder(ctx, e.OutTradeNo, "manual_confirm_not_paid")
		if cerr != nil {
			_ = h.store.UpdateException(ctx, id, func(up *model.ExceptionOrder) { up.Phase = model.ExPhasePending })
			c.JSON(http.StatusInternalServerError, gin.H{"error": "关单失败: " + cerr.Error()})
			return
		}
		if !ok {
			_ = h.store.UpdateException(ctx, id, func(up *model.ExceptionOrder) { up.Phase = model.ExPhasePending })
			c.JSON(http.StatusConflict, gin.H{"error": "订单状态不允许关闭（可能已支付）"})
			return
		}
		_ = h.store.UpdateOrderHandled(ctx, e.OutTradeNo, id, "manual_close", operator, req.Note)
		result = "已确认未支付并关闭"
	case model.ExActionNoop:
		_ = h.store.UpdateOrderHandled(ctx, e.OutTradeNo, id, "noop", operator, req.Note)
		result = "无需处理，已记录"
	}

	_ = h.store.UpdateException(ctx, id, func(up *model.ExceptionOrder) {
		up.Phase = model.ExPhaseResolved
		up.ResolvedAction = req.Action
		up.Handler = operator
		up.HandlerNote = req.Note
		if req.PaidAmount > 0 {
			up.PaidAmount = req.PaidAmount
		}
		if req.ChannelTxID != "" {
			up.ChannelTxID = req.ChannelTxID
		}
		up.ResolvedAt = time.Now().Format(time.RFC3339)
	})
	detail := fmt.Sprintf("action=%s note=%q", req.Action, req.Note)
	_ = h.store.InsertExceptionLog(ctx, id, "resolve", operator, detail)
	logger.Warn("admin", "异常单人工处理完成", "exception_id", id, "out_trade_no", e.OutTradeNo, "action", req.Action, "operator", operator)
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"result": result}})
}

// handleReopen 重新打开异常单（处理后发现问题）
func (h *ReconcilerHandler) handleReopen(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	operator := c.GetString("username")
	err := h.store.UpdateException(c.Request.Context(), id, func(up *model.ExceptionOrder) {
		up.Phase = model.ExPhasePending
		up.ResolvedAction = ""
		up.ResolvedAt = ""
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	_ = h.store.InsertExceptionLog(c.Request.Context(), id, "reopen", operator, "")
	c.JSON(http.StatusOK, gin.H{"data": "已重新打开"})
}

// handleManualAdd 手动加入异常池
func (h *ReconcilerHandler) handleManualAdd(c *gin.Context) {
	var req struct {
		OutTradeNo string `json:"out_trade_no"`
		Note       string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.OutTradeNo == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "参数错误: 需要 out_trade_no"})
		return
	}
	o, err := h.store.GetOrder(c.Request.Context(), req.OutTradeNo)
	if err != nil || o == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "订单不存在"})
		return
	}
	h.svc.EnqueueException(c.Request.Context(), o, model.ExReasonManual, o.Status, "", "{}", req.Note)
	operator := c.GetString("username")
	// 记录 operator
	if e, err := h.store.GetExceptionByOutTradeNo(c.Request.Context(), req.OutTradeNo); err == nil && e != nil {
		_ = h.store.InsertExceptionLog(c.Request.Context(), e.ID, "manual_add", operator, req.Note)
	}
	c.JSON(http.StatusOK, gin.H{"data": "已加入异常池"})
}

// ========== dead 通知管理 ==========

func (h *ReconcilerHandler) handleListDead(c *gin.Context) {
	tasks, err := h.store.ListNotifyTasks(c.Request.Context(), "dead", 100)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": gin.H{"list": tasks, "total": len(tasks)}})
}

// handleRetryDead 重发 dead 通知（重置为 pending，重试计数清零）。
// 重发前按订单当前 NotifyURL 重建 payload 并重新签名，解决「配置时回调地址错误 → 重发仍打旧地址 → 一直回调不了」。
func (h *ReconcilerHandler) handleRetryDead(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	operator := c.GetString("username")
	ctx := c.Request.Context()

	task, err := h.store.GetNotifyTask(ctx, id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if task == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "回调任务不存在"})
		return
	}
	// 用订单最新 NotifyURL 重建 payload；订单丢失/svc 缺失则保留原 payload（避免丢通知）
	payload := task.Payload
	if h.svc != nil {
		if o, oerr := h.store.GetOrderByID(ctx, task.OrderID); oerr == nil && o != nil {
			payload = h.svc.BuildNotifyPayload(o)
		}
	}
	if err := h.store.UpdateNotifyTaskPayload(ctx, id, payload); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if err := h.store.UpdateNotifyTask(ctx, id, "pending", 0, time.Now()); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Warn("admin", "dead 通知重发（已刷新为最新回调地址）", "task_id", id, "operator", operator)
	c.JSON(http.StatusOK, gin.H{"data": "已按最新回调地址重新入队"})
}

// handleDeleteDead 彻底删除 dead 通知任务（确认放弃该笔通知；若订单已支付但未通知，删除后无法自动补发）
func (h *ReconcilerHandler) handleDeleteDead(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	operator := c.GetString("username")
	if err := h.store.DeleteNotifyTask(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Warn("admin", "dead 通知已删除", "task_id", id, "operator", operator)
	c.JSON(http.StatusOK, gin.H{"data": "已删除"})
}

// ========== 日终对账报告（RESILIENCE P2-1） ==========

// handleDailyReport 实时生成日终对账报告
// GET /api/admin/reconciler/daily-report?date=YYYY-MM-DD（空=今天）
func (h *ReconcilerHandler) handleDailyReport(c *gin.Context) {
	report, err := reconciler.BuildDailyReport(c.Request.Context(), h.store, c.Query("date"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": report})
}

var _ = context.Background // keep import if unused
