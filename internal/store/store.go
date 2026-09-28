// Package store 订单存储抽象：接口 + SQLite 实现
package store

import (
	"context"
	"time"

	"paygateway/internal/model"
)

// Store 存储接口：资金数据必须走这里（SQLite/PG 可切换）
type Store interface {
	// 订单
	CreateOrder(ctx context.Context, o *model.Order) error
	// DeleteOrder 删除订单；仅当状态为 WAIT_BUYER_PAY 时生效并返回 true
	DeleteOrder(ctx context.Context, outTradeNo string) (bool, error)
	GetOrder(ctx context.Context, outTradeNo string) (*model.Order, error)
	GetOrderByID(ctx context.Context, id int64) (*model.Order, error)
	// 条件更新：from -> to，影响行数=0 表示状态非法（幂等/防覆盖）
	UpdateOrderStatus(ctx context.Context, outTradeNo, from, to string, set func(o *model.Order)) (bool, error)
	// ReplaceOrder 买家替换：原子关闭 WAIT_PAY -> CLOSED 并在同一事务写 replaced 事件（见实现，关单与事件同事务持久化）。
	ReplaceOrder(ctx context.Context, outTradeNo string, orderID int64, amountMinor int64, rawJSON string) (bool, error)
	// UpdateOrderAmount 更新订单金额（复用订单时以最新下单金额覆盖，防实付与订单金额不一致导致回调失败）
	UpdateOrderAmount(ctx context.Context, outTradeNo string, amountMinor int64) error
	// ListExpiredOrders 查询超过 before 时间仍未支付的订单（自动关闭用）
	ListExpiredOrders(ctx context.Context, before time.Time) ([]*model.Order, error)
	// 个人免签：按备注码查询未支付订单（匹配引擎用）
	GetOrderByNoteCode(ctx context.Context, noteCode string, since time.Time) (*model.Order, error)
	// 个人免签：查询某时间窗口内同金额的未支付订单（金额兜底匹配）
	GetOrdersByAmount(ctx context.Context, amountMinor int64, since time.Time) ([]*model.Order, error)
	// 个人免签：查询某时间窗口内同金额的已支付订单（重复捕获判定：通知栏/账本双链 5s 内同时捕获同一笔收款，败者零 WAIT_PAY 候选）
	// accountIDs 限定待匹配的账户集合（当前设备可服务的账户）；同金额但属其他设备/账户的已支付订单不纳入，
	// 避免「通知栏已入账的那笔」被账单页误判为无订单收款。空集合 = 不过滤全查。
	GetRecentSettledByAmount(ctx context.Context, amountMinor int64, since time.Time, accountIDs []int64) ([]*model.Order, error)
	// 个人免签：按套码 token 查询订单
	GetOrderByQrToken(ctx context.Context, qrToken string) (*model.Order, error)
	// 个人免签：查询指定收款账户下所有待支付订单（监听端离线联动：进异常池人工核实）
	ListWaitPayByAccount(ctx context.Context, accountID int64) ([]*model.Order, error)
	// 个人免签：查询全部待支付订单（WaitPayPool 启动重建/对账用）
	ListWaitPayAll(ctx context.Context) ([]*model.Order, error)
	// 个人免签：查询指定买家环境(buyer_key)下所有待支付订单（买家级串行：一个环境同时只允许一笔待支付）
	ListWaitPayByBuyer(ctx context.Context, buyerKey string) ([]*model.Order, error)
	// 个人免签：按买家环境(buyer_key)或浏览器指纹(fp_key)查询待支付订单（指纹绑定后串行检查兜底）
	ListWaitPayByBuyerOrFP(ctx context.Context, buyerKey, fpKey string) ([]*model.Order, error)

	// ---------- 系统设置 ----------
	GetSetting(ctx context.Context, key string) (string, error)
	SetSetting(ctx context.Context, key, value string) error
	DeleteSetting(ctx context.Context, key string) error
	GetAllSettings(ctx context.Context) (map[string]string, error)
	MarkNotified(ctx context.Context, outTradeNo string, notifyCount int) error
	ListOrders(ctx context.Context, status, channel string, limit, offset int) ([]*model.Order, error)
	CountOrders(ctx context.Context, status string) (int64, error)
	SumAmount(ctx context.Context, status string) (int64, error)
	// 增强查询：keyword 搜索订单号，from/to 时间范围（yyyy-mm-dd）
	ListOrdersFiltered(ctx context.Context, status, channel, keyword, from, to string, limit, offset int) ([]*model.Order, error)
	CountOrdersFiltered(ctx context.Context, status, channel, keyword, from, to string) (int64, error)
	CountOrdersByChannel(ctx context.Context, status, channel string) (int64, error)
	SumAmountByChannel(ctx context.Context, status, channel string) (int64, error)
	CountOrdersByChannelToday(ctx context.Context, channel, date string) (int64, error)
	SumAmountByChannelToday(ctx context.Context, channel, date string) (int64, error)
	// 日终对账：按通道+状态+日期统计（RESILIENCE P2-1）
	DailyStatsByChannel(ctx context.Context, channel, status, date string) (count, amount int64, err error)
	// 按天趋势：返回最近 days 天（含今天）每日订单统计
	OrdersTrend(ctx context.Context, start, end string) ([]DailyStat, error)

	// 资金流水（只增不改，审计基础）
	InsertEvent(ctx context.Context, orderID int64, eventType, channelTxID string, amountMinor int64, rawPayload string) error
	ListEvents(ctx context.Context, orderID int64, limit int) ([]*PaymentEvent, error)

	// 回调任务（outbox）
	CreateNotifyTask(ctx context.Context, orderID int64, payload string) (int64, error)
	GetPendingNotifyTasks(ctx context.Context, limit int) ([]*NotifyTask, error)
	GetNotifyTask(ctx context.Context, id int64) (*NotifyTask, error)
	UpdateNotifyTask(ctx context.Context, id int64, status string, retryCount int, nextRetryAt time.Time) error
	// UpdateNotifyTaskPayload 更新回调任务 payload（死信重发时按订单最新 notify_url 重建）
	UpdateNotifyTaskPayload(ctx context.Context, id int64, payload string) error
	// DeleteNotifyTask 删除回调任务（彻底移除死信，确认放弃该笔通知）
	DeleteNotifyTask(ctx context.Context, id int64) error
	ListNotifyTasks(ctx context.Context, status string, limit int) ([]*NotifyTask, error)
	CountNotifyTasks(ctx context.Context, status string) (int64, error)

	// 日志落库（管理界面查询）
	InsertLog(ctx context.Context, level, scope, message, traceID, fields string) error
	QueryLogs(ctx context.Context, level, scope string, limit, offset int) ([]*LogRecord, error)
	CountLogs(ctx context.Context, level, scope string) (int64, error)

	// ---------- 人工异常处理池 ----------
	CreateException(ctx context.Context, e *model.ExceptionOrder) error
	GetException(ctx context.Context, id int64) (*model.ExceptionOrder, error)
	GetExceptionByOutTradeNo(ctx context.Context, outTradeNo string) (*model.ExceptionOrder, error)
	ListExceptions(ctx context.Context, phase, reason string, limit, offset int) ([]*model.ExceptionOrder, error)
	CountExceptions(ctx context.Context, phase string) (int64, error)
	UpdateException(ctx context.Context, id int64, set func(e *model.ExceptionOrder)) error
	UpdateExceptionPhase(ctx context.Context, id int64, from, to string) (bool, error)
	InsertExceptionLog(ctx context.Context, exceptionID int64, action, operator, detail string) error
	ListExceptionLogs(ctx context.Context, exceptionID int64, limit int) ([]*model.ExceptionLog, error)
	// UpdateOrderHandled 订单人工处理标记（人工池处理后永久留痕）
	UpdateOrderHandled(ctx context.Context, outTradeNo string, exceptionID int64, action, handler, note string) error

	Close() error
}

// DailyStat 按天订单统计（趋势图用）
type DailyStat struct {
	Date          string `json:"date"`           // 2006-01-02
	TotalOrders   int64  `json:"total_orders"`   // 当日全部订单
	SuccessOrders int64  `json:"success_orders"` // 当日成功订单
	AmountMinor   int64  `json:"amount_minor"`   // 当日成功金额（分）
}

// PaymentEvent 资金流水
type PaymentEvent struct {
	ID          int64     `json:"id"`
	OrderID     int64     `json:"order_id"`
	EventType   string    `json:"event_type"`
	ChannelTxID string    `json:"channel_tx_id"`
	AmountMinor int64     `json:"amount_minor"`
	RawPayload  string    `json:"raw_payload"`
	CreatedAt   time.Time `json:"created_at"`
}

// NotifyTask 回调任务（outbox）
type NotifyTask struct {
	ID          int64     `json:"id"`
	OrderID     int64     `json:"order_id"`
	Payload     string    `json:"payload"`
	Status      string    `json:"status"` // pending / success / dead
	RetryCount  int       `json:"retry_count"`
	NextRetryAt time.Time `json:"next_retry_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// LogRecord 日志记录
type LogRecord struct {
	ID        int64     `json:"id"`
	Level     string    `json:"level"`
	Scope     string    `json:"scope"`
	Message   string    `json:"message"`
	TraceID   string    `json:"trace_id"`
	Fields    string    `json:"fields"`
	CreatedAt time.Time `json:"created_at"`
}

// LedgerEntry 账本条目
type LedgerEntry struct {
	ID          int64     `json:"id"`
	PrevHash    string    `json:"prev_hash"`
	PayloadHash string    `json:"payload_hash"`
	EntryHash   string    `json:"entry_hash"`
	Signature   string    `json:"signature"`
	EventID     int64     `json:"event_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// Snapshot 每日快照
type Snapshot struct {
	ID           int64
	SnapshotDate string
	LastHash     string
	Signature    string
	CreatedAt    time.Time
}
