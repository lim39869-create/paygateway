// Package notify 可靠回调投递（Outbox）：worker 扫描 pending 任务 → POST 接入方 → 重试
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"paygateway/internal/alert"
	"paygateway/internal/logger"
	"paygateway/internal/store"
)

// Worker 回调投递 worker
type Worker struct {
	store    store.Store
	client   *http.Client
	interval time.Duration
	// RetryDelays 重试间隔（指数退避）
	RetryDelays []time.Duration
	// MaxRetries 最大重试次数（超过标记 dead）
	MaxRetries int
	alert      *alert.Client // 告警（RESILIENCE P1-3，nil=禁用）
}

// SetAlert 注入告警客户端（可选）
func (w *Worker) SetAlert(a *alert.Client) { w.alert = a }

// NewWorker 创建回调 worker。allowPrivateNotify=false 时启用 SSRF 复核拨号器。
func NewWorker(st store.Store, allowPrivateNotify bool) *Worker {
	return &Worker{
		store:    st,
		client:   NewNotifyHTTPClient(15*time.Second, allowPrivateNotify),
		interval: 3 * time.Second,
		RetryDelays: []time.Duration{
			1 * time.Minute, 5 * time.Minute, 15 * time.Minute,
			1 * time.Hour, 6 * time.Hour, 24 * time.Hour,
		},
		MaxRetries: 6, // 总窗口 ≈ 31h，覆盖上游长时间不可用
	}
}

// Start 启动循环（阻塞，应放 goroutine）
func (w *Worker) Start(ctx context.Context) {
	logger.Info("notify", "回调 worker 启动", "interval", w.interval.String())
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("notify", "回调 worker 停止")
			return
		case <-ticker.C:
			w.processBatch(ctx)
		}
	}
}

// processBatch 处理一批到期任务
func (w *Worker) processBatch(ctx context.Context) {
	tasks, err := w.store.GetPendingNotifyTasks(ctx, 20)
	if err != nil {
		logger.Error("notify", "获取回调任务失败", "err", err)
		return
	}
	for _, task := range tasks {
		w.process(ctx, task)
	}
}

// process 处理单个任务
func (w *Worker) process(ctx context.Context, task *store.NotifyTask) {
	// 解析 payload（易支付协议参数，已含签名）
	var params map[string]string
	if err := json.Unmarshal([]byte(task.Payload), &params); err != nil {
		// payload 损坏：重试也无效，标记 dead
		logger.Error("notify", "回调任务 payload 损坏，标记 dead", "task_id", task.ID, "err", err)
		w.store.UpdateNotifyTask(ctx, task.ID, "dead", task.RetryCount, time.Now())
		return
	}

	notifyURL := params["notify_url"]
	if notifyURL == "" {
		// 没有回调地址（payload 里应该有），标记成功跳过
		w.store.UpdateNotifyTask(ctx, task.ID, "success", task.RetryCount, time.Now())
		return
	}
	// 注意：notify_url 保留在 body 中并参与签名。
	// New API（go-epay）验签逻辑 = 用收到的全部参数重算签名比对，
	// 若删除 notify_url 会导致签名不一致 → 验签失败。易支付标准签名本就包含 notify_url。

	body := buildForm(params)
	start := time.Now()
	resp, err := w.client.Post(notifyURL, "application/x-www-form-urlencoded", bytes.NewReader(body))
	latency := time.Since(start).Milliseconds()

	if err != nil {
		w.retryOrDead(ctx, task, "请求失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	logger.Info("notify", "回调接入方",
		"task_id", task.ID,
		"out_trade_no", params["out_trade_no"],
		"pid", params["pid"],
		"url", notifyURL,
		"status", resp.StatusCode,
		"latency_ms", latency)

	// 易支付协议：商户处理成功必须响应正文 "success"。
	// 仅凭 HTTP 200 判定会把 LB/网关的错误页、鉴权墙当成功，导致上游永远收不到有效通知。
	if resp.StatusCode == 200 && strings.EqualFold(strings.TrimSpace(string(respBody)), "success") {
		logger.Info("notify", "回调成功 ✅", "task_id", task.ID)
		w.store.UpdateNotifyTask(ctx, task.ID, "success", task.RetryCount, time.Now())
		// 同步更新订单通知状态（管理后台展示）
		if otn := params["out_trade_no"]; otn != "" {
			if err := w.store.MarkNotified(ctx, otn, task.RetryCount+1); err != nil {
				logger.Warn("notify", "更新订单通知状态失败", "err", err, "out_trade_no", otn)
			}
		}
	} else if resp.StatusCode == 200 {
		logger.Warn("notify", "回调 HTTP 200 但响应非 success（视为失败重试）",
			"task_id", task.ID, "body", string(respBody))
		w.retryOrDead(ctx, task, "200 但响应体非 success")
	} else {
		logger.Warn("notify", "回调失败", "task_id", task.ID, "status", resp.StatusCode, "body", string(respBody))
		w.retryOrDead(ctx, task, resp.Status)
	}
}

// retryOrDead 重试或标记 dead
func (w *Worker) retryOrDead(ctx context.Context, task *store.NotifyTask, reason string) {
	retryCount := task.RetryCount + 1
	if retryCount > w.MaxRetries {
		logger.Error("notify", "回调任务超过最大重试，标记 dead", "task_id", task.ID, "reason", reason)
		w.store.UpdateNotifyTask(ctx, task.ID, "dead", retryCount, time.Now())
		// 告警：通知 dead（RESILIENCE P1-3）
		if w.alert != nil {
			w.alert.Send("error", "回调任务dead",
				fmt.Sprintf("订单 %s 回调超过最大重试标记 dead（%d 次）\n原因: %s", taskPayloadTradeNo(task), retryCount, reason))
		}
		return
	}
	delay := time.Minute
	if retryCount-1 < len(w.RetryDelays) {
		delay = w.RetryDelays[retryCount-1]
	}
	next := time.Now().Add(delay)
	logger.Warn("notify", "回调任务将重试", "task_id", task.ID, "retry", retryCount, "next", next.Format(time.RFC3339))
	w.store.UpdateNotifyTask(ctx, task.ID, "pending", retryCount, next)
}

// taskPayloadTradeNo 从任务 payload 提取订单号（告警信息用，失败返回空串）
func taskPayloadTradeNo(task *store.NotifyTask) string {
	var params map[string]string
	if err := json.Unmarshal([]byte(task.Payload), &params); err != nil {
		return ""
	}
	return params["out_trade_no"]
}

// buildForm 构造表单 body（值做 URL 编码，兼容中文/特殊字符）
func buildForm(params map[string]string) []byte {
	var buf bytes.Buffer
	for k, v := range params {
		buf.WriteString(url.QueryEscape(k) + "=" + url.QueryEscape(v) + "&")
	}
	s := buf.String()
	if len(s) > 0 {
		s = s[:len(s)-1]
	}
	return []byte(s)
}
