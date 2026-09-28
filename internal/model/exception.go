package model

// ExceptionOrder 人工异常处理池订单（自动化兜底失败后的最终兜底）
// 只增不改：resolved 后可 reopen，不可删除；全程审计（exception_logs）
type ExceptionOrder struct {
	ID             int64  `json:"id"`
	OutTradeNo     string `json:"out_trade_no"`
	Pid            string `json:"pid"`
	UpstreamName   string `json:"upstream_name"`
	Channel        string `json:"channel"`
	AmountMinor    int64  `json:"amount_minor"`
	OrderStatus    string `json:"order_status"`
	ChannelStatus  string `json:"channel_status"` // TRADE_SUCCESS / TRADE_CLOSED / QUERY_FAILED / UNKNOWN
	ChannelTxID    string `json:"channel_tx_id"`
	PaidAmount     int64  `json:"paid_amount"` // 人工确认的实际支付金额（默认=AmountMinor）
	Payer          string `json:"payer"`
	Reason         string `json:"reason"`
	Phase          string `json:"phase"` // pending / processing / resolved
	ResolvedAction string `json:"resolved_action"`
	Evidence       string `json:"evidence"` // 证据时间线 JSON（自动收集，只读）
	Suggestion     string `json:"suggestion"`
	Handler        string `json:"handler"`
	HandlerNote    string `json:"handler_note"`
	ResolvedAt     string `json:"resolved_at"`
	UpdatedAt      string `json:"updated_at"`
	CreatedAt      string `json:"created_at"`
}

// ExceptionLog 异常池操作审计
type ExceptionLog struct {
	ID          int64  `json:"id"`
	ExceptionID int64  `json:"exception_id"`
	Action      string `json:"action"`
	Operator    string `json:"operator"`
	Detail      string `json:"detail"`
	CreatedAt   string `json:"created_at"`
}

// 异常原因（自动进池触发点）
const (
	ExReasonQueryFailed    = "query_failed"    // 查单连续失败，无法确认
	ExReasonAmountMismatch = "amount_mismatch" // 查单金额 ≠ 订单金额
	ExReasonStateConflict  = "state_conflict"  // 回调与查单状态矛盾
	ExReasonCloseUncertain = "close_uncertain" // 关单时查单失败，存疑
	ExReasonNotifyDead     = "notify_dead"     // 通知 dead 且重发失败
	ExReasonManual         = "manual"          // 管理后台手动加入
	ExReasonListenOffline  = "listen_offline"  // 监听端掉线，无法确认是否已支付（个人通道）
	ExReasonListenTimeout  = "listen_timeout"  // 监听端在线但超时无推送，存疑（个人通道）
)

// 异常订单阶段
const (
	ExPhasePending    = "pending"
	ExPhaseProcessing = "processing"
	ExPhaseResolved   = "resolved"
)

// 人工处理动作（resolved_action）
const (
	ExActionSettle = "settle" // 确认入账
	ExActionClose  = "close"  // 确认未支付，关单
	ExActionNoop   = "noop"   // 无需处理
)
