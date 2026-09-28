# dead 通知手动重试(用最新回调地址) + 删除 实施设计（DESIGN-NOTIFY-RETRY-LATEST-URL）

> 状态：**已实现**（`order.Service.BuildNotifyPayload` 单一来源；`handleRetryDead` 重发前按订单最新
> NotifyURL 重建 payload 并重新签名；新增 `handleDeleteDead` + `DeleteNotifyTask`；store 新增
> `GetNotifyTask`/`UpdateNotifyTaskPayload`；Reconciler.vue 死信页增删除按钮，NotifyTasks.vue 手动重试接入真实 API）
> 关联：docs/RESILIENCE.md（对账兜底）、docs/ARCHITECTURE.md（outbox 回调队列）
> 影响模块：`internal/order`、`internal/store`、`internal/admin/reconciler`、`web/admin`
> 目标：**死信/失败回调任务手动重试时，用订单当前最新的 notify_url 重建 payload 并重新签名投递，
>        解决「配置时 notify_url 错误 → 重发仍打到旧地址 → 一直回调不了 → 死信」；并提供彻底删除死信任务的能力。**

---

## 1. 背景与问题（现状代码事实）

### 1.1 Outbox 回调队列现状

- 生产者 `order.Service.EnqueueNotify`（internal/order/service.go:1126）：支付成功/人工入账后调用，
  构造易支付协议参数（含 `notify_url` = `o.NotifyURL`），`epay.BuildSignedParams(params, signKey(pid))`
  签名，JSON 序列化为 `payload`，`CreateNotifyTask` 落库。
- 消费者 `notify.Worker`（internal/notify/worker.go）：`process` 解析 payload，
  `notifyURL := params["notify_url"]`（worker.go:88）作为发送地址，body = 全部 params（含签名）。
  超过 `MaxRetries=6` 标记 `dead`（worker.go:142-144）。
- 状态：`pending` / `success` / `dead`（store.go:121）。

### 1.2 关键缺陷：payload 固化 notify_url，重放不刷新

- `EnqueueNotify` 在**任务创建时**把 `o.NotifyURL` 写进 payload。
- worker 发送从 **payload** 读 `notify_url`，不是订单当前值。
- 现有死信重发 `admin.Handler.handleRetryDead`（internal/admin/reconciler.go:287）只调
  `UpdateNotifyTask(ctx, id, "pending", 0, now)`——仅更新 `status/retry_count/next_retry_at/updated_at`，
  **完全不碰 payload**。因此死信重发仍打到**旧地址**。
- 后果：商户配置时把 notify_url 配错，或之后改了回调地址，死信重发永远打到旧地址 → 一直回调不了。

### 1.3 现状已有 vs 缺失

| 能力 | 现状 |
|---|---|
| 死信列表 | 已有 `GET /api/reconciler/notify-dead`（handleListDead） |
| 死信重发 | 已有 `POST /api/reconciler/notify-dead/:id/retry`，但**不刷新地址** |
| NotifyTasks.vue 手动重试 | **占位**（web/admin/src/views/NotifyTasks.vue:123-126，只弹提示） |
| 彻底删除死信 | **完全没有**（无 DeleteNotifyTask / DELETE 路由） |

---

## 2. 设计决策

### 2.1 重试用订单最新 notify_url（核心）

重发前：通过 `task.OrderID` → `GetOrderByID` 取订单，**用订单当前 `NotifyURL` 重建 payload 并重新签名**，
再重置为 `pending`。理由：
- worker 发送地址 = payload 里的 `notify_url`，所以必须重建 payload 才能换地址。
- 商户端按"收到的全部参数重算签名"验签（worker.go:94-96 注释），改了 notify_url 必须**重新签名**，
  否则验签失败。因此必须复用 `EnqueueNotify` 的完整构造+签名逻辑，不能只改 notify_url 字符串。

**单一来源重构**：把 `EnqueueNotify` 的 payload 构造逻辑提取为 `Service` 的导出方法
`BuildNotifyPayload(o *model.Order) string`，`EnqueueNotify` 与重试 handler 都调用它，
保证字段与签名完全一致、单一来源。

### 2.2 彻底删除死信任务

提供 `DELETE` 语义：新增 `handleDeleteDead`（`POST /reconciler/notify-dead/:id/delete`）。
**风险提示**：若订单已支付但通知未送达，删除后无法再自动补发（人工可重新入队兜底，见下）。属"确认放弃该笔通知"操作。

### 2.3 前端

- `Reconciler.vue` 死信页：现有"重发"按钮后端已会刷新地址（无需前端改逻辑）；新增"删除"按钮（二次确认）。
- `NotifyTasks.vue`：修正占位手动重试——复用 `retryDeadNotify`/`delete`，接入真实后端；或移除占位避免误导。
  （最小改动：让占位按钮调用真实 retry API。）

---

## 3. 实现细节

### 3.1 `internal/order/service.go` — payload 构造单一来源

新增导出方法（含签名），`EnqueueNotify` 内部复用它：

```go
// BuildNotifyPayload 构造易支付协议回调参数并签名，返回 JSON payload。
// 供 EnqueueNotify（下单入队）与死信手动重发（用订单最新 notify_url）复用，保证字段/签名单一来源。
func (s *Service) BuildNotifyPayload(o *model.Order) string {
	params := map[string]string{
		"trade_no":     o.GatewayNo,
		"out_trade_no": o.OutTradeNo,
		"type":         o.Channel,
		"name":         o.Subject,
		"money":        o.MoneyYuan(),
		"trade_status": "TRADE_SUCCESS",
		"notify_url":   o.NotifyURL,
	}
	if o.Attach != "" {
		params["attach"] = o.Attach
	}
	if o.ChannelTxID != "" {
		params["trade_no"] = o.ChannelTxID
	}
	epay.BuildSignedParams(params, s.signKey(o.Pid))
	payload, _ := json.Marshal(params)
	return string(payload)
}

func (s *Service) EnqueueNotify(ctx context.Context, o *model.Order) {
	payload := s.BuildNotifyPayload(o)
	taskID, err := s.store.CreateNotifyTask(ctx, o.ID, payload)
	// ...原有逻辑不变
}
```

### 3.2 `internal/store/store.go` — 接口新增

```go
// UpdateNotifyTaskPayload 更新回调任务 payload（死信重发时按订单最新 notify_url 重建）
UpdateNotifyTaskPayload(ctx context.Context, id int64, payload string) error
// DeleteNotifyTask 删除回调任务（彻底移除死信，确认放弃该笔通知）
DeleteNotifyTask(ctx context.Context, id int64) error
```

### 3.3 `internal/store/sqlite_rest.go` — 实现

```go
func (s *SQLiteStore) UpdateNotifyTaskPayload(ctx context.Context, id int64, payload string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE notify_tasks SET payload=?, updated_at=? WHERE id=?`, payload, nowStr(), id)
	return err
}

func (s *SQLiteStore) DeleteNotifyTask(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notify_tasks WHERE id=?`, id)
	return err
}
```

### 3.4 `internal/admin/reconciler.go` — 重发刷新地址 + 删除

```go
func (h *ReconcilerHandler) handleRetryDead(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	operator := c.GetString("username")
	ctx := c.Request.Context()

	// 取任务
	tasks, err := h.store.ListNotifyTasks(ctx, "dead", 100) // 或新增 GetNotifyTask(id)
	// （List 按 id DESC，需从列表定位 task；更干净：新增 GetNotifyTask(id)）
	var task *store.NotifyTask
	for _, t := range tasks { if t.ID == id { task = t; break } }
	if task == nil { c.JSON(404, ...); return }

	// 取订单，用最新 NotifyURL 重建 payload + 重签
	o, err := h.store.GetOrderByID(ctx, task.OrderID)
	var payload string
	if err != nil || o == nil {
		// 订单丢失：保留原 payload（无法重建），仅重置 pending
		payload = task.Payload
	} else {
		payload = h.svc.BuildNotifyPayload(o)
	}

	if err := h.store.UpdateNotifyTaskPayload(ctx, id, payload); err != nil { ... }
	h.store.UpdateNotifyTask(ctx, id, "pending", 0, time.Now())
	logger.Warn("admin", "dead 通知重发（用最新 notify_url）", "task_id", id, "operator", operator)
	c.JSON(http.StatusOK, gin.H{"data": "已按最新回调地址重新入队"})
}

func (h *ReconcilerHandler) handleDeleteDead(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	operator := c.GetString("username")
	if err := h.store.DeleteNotifyTask(c.Request.Context(), id); err != nil { ... }
	logger.Warn("admin", "dead 通知已删除", "task_id", id, "operator", operator)
	c.JSON(http.StatusOK, gin.H{"data": "已删除"})
}
```

> 更干净：store 新增 `GetNotifyTask(ctx, id)` 避免在列表中扫描定位。**设计决策：新增 `GetNotifyTask`**。

### 3.5 路由

```go
rg.POST("/reconciler/notify-dead/:id/retry", h.handleRetryDead)
rg.POST("/reconciler/notify-dead/:id/delete", h.handleDeleteDead) // 新增
```

### 3.6 前端 `api.js` + `Reconciler.vue`

```js
// api.js
deleteDeadNotify: (id) => request(`/reconciler/notify-dead/${id}/delete`, { method: 'POST' }),
```

`Reconciler.vue` 死信行操作区加"删除"按钮（ElMessageBox.confirm 二次确认），
重发按钮保持（后端已刷新地址）。

### 3.7 `NotifyTasks.vue` 占位手动重试

将 `manualRetry` 占位改为调用真实 retry API（当前选中任务 `current.id`）：

```js
async function manualRetry() {
  try { await api.retryDeadNotify(current.value.id); ElMessage.success('已按最新回调地址重新入队'); }
  catch (e) { ElMessage.error('重试失败: ' + e.message) }
  finally { payloadDialog.value = false; load() }
}
```

> 依赖 api.js 暴露 `retryDeadNotify`（已有，api.js:148）。

---

## 4. 边界与风险

| 场景 | 处理 | 风险 |
|---|---|---|
| 订单已删除/丢失 | 无法重建 payload → 保留原 payload 仅重置 pending | 仍打旧地址；提示人工 |
| 订单 NotifyURL 为空 | 重建后 notify_url 空 → worker 标记 success 跳过 | 极端；需人工 |
| 删除已支付但未通知的死信 | 不再自动补发，人工可从异常池/重新入队 | 资金通知可能丢失——删除前二次确认 |
| 并发（worker 同时取到同任务重试） | 重置 pending 后 next_retry_at=now，worker 下一轮即发 | 重复投递一次，幂等可接受 |
| NotifyTasks 全量页与 Reconciler 死信页 | 两处都接入真实 API | 保持一致 |

---

## 5. 回归测试

- `internal/store`:新增 `UpdateNotifyTaskPayload` / `DeleteNotifyTask` 的单测（落库、读回、删除）。
- `internal/admin`:`handleRetryDead` 重建 payload 用最新 notify_url（伪造 task+order，断言 payload 内 notify_url 为最新值且签名有效）。
- `internal/order`:`BuildNotifyPayload` 与 `EnqueueNotify` 产物一致（单一来源）。
