package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"paygateway/internal/model"
)

// ---------- 订单查询 ----------

func (s *SQLiteStore) ListOrders(ctx context.Context, status, channel string, limit, offset int) ([]*model.Order, error) {
	q := `SELECT ` + orderColumns + ` FROM orders WHERE 1=1`
	args := []any{}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	if channel != "" {
		q += " AND channel=?"
		args = append(args, channel)
	}
	q += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) ListExpiredOrders(ctx context.Context, before time.Time) ([]*model.Order, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE status=? AND created_at <= ? ORDER BY id`,
		model.StatusWaitPay, before.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) CountOrders(ctx context.Context, status string) (int64, error) {
	q := `SELECT COUNT(*) FROM orders`
	args := []any{}
	if status != "" {
		q += " WHERE status=?"
		args = append(args, status)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *SQLiteStore) SumAmount(ctx context.Context, status string) (int64, error) {
	q := `SELECT COALESCE(SUM(amount_minor),0) FROM orders`
	args := []any{}
	if status != "" {
		q += " WHERE status=?"
		args = append(args, status)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *SQLiteStore) ListOrdersFiltered(ctx context.Context, status, channel, keyword, from, to string, limit, offset int) ([]*model.Order, error) {
	q := `SELECT ` + orderColumns + ` FROM orders WHERE 1=1`
	args := []any{}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	if channel != "" {
		q += " AND channel=?"
		args = append(args, channel)
	}
	if keyword != "" {
		q += " AND (out_trade_no LIKE ? OR gateway_no LIKE ? OR channel_tx_id LIKE ? OR subject LIKE ?)"
		kw := "%" + keyword + "%"
		args = append(args, kw, kw, kw, kw)
	}
	if from != "" {
		q += " AND substr(created_at,1,10) >= ?"
		args = append(args, from)
	}
	if to != "" {
		q += " AND substr(created_at,1,10) <= ?"
		args = append(args, to)
	}
	q += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) CountOrdersFiltered(ctx context.Context, status, channel, keyword, from, to string) (int64, error) {
	q := `SELECT COUNT(*) FROM orders WHERE 1=1`
	args := []any{}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	if channel != "" {
		q += " AND channel=?"
		args = append(args, channel)
	}
	if keyword != "" {
		q += " AND (out_trade_no LIKE ? OR gateway_no LIKE ? OR channel_tx_id LIKE ? OR subject LIKE ?)"
		kw := "%" + keyword + "%"
		args = append(args, kw, kw, kw, kw)
	}
	if from != "" {
		q += " AND substr(created_at,1,10) >= ?"
		args = append(args, from)
	}
	if to != "" {
		q += " AND substr(created_at,1,10) <= ?"
		args = append(args, to)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *SQLiteStore) CountOrdersByChannel(ctx context.Context, status, channel string) (int64, error) {
	q := `SELECT COUNT(*) FROM orders WHERE channel=?`
	args := []any{channel}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *SQLiteStore) SumAmountByChannel(ctx context.Context, status, channel string) (int64, error) {
	q := `SELECT COALESCE(SUM(amount_minor),0) FROM orders WHERE channel=?`
	args := []any{channel}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *SQLiteStore) CountOrdersByChannelToday(ctx context.Context, channel, date string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM orders WHERE channel=? AND substr(created_at,1,10)=?`,
		channel, date).Scan(&n)
	return n, err
}

func (s *SQLiteStore) SumAmountByChannelToday(ctx context.Context, channel, date string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(amount_minor),0) FROM orders WHERE channel=? AND status='TRADE_SUCCESS' AND substr(created_at,1,10)=?`,
		channel, date).Scan(&n)
	return n, err
}

// DailyStatsByChannel 日终对账：按通道+状态+日期统计订单数与金额
// status 传 "" 表示全部状态；date 格式 2006-01-02（与 created_at 前 10 位比对）
func (s *SQLiteStore) DailyStatsByChannel(ctx context.Context, channel, status, date string) (int64, int64, error) {
	q := `SELECT COUNT(*), COALESCE(SUM(amount_minor),0) FROM orders
	      WHERE channel=? AND substr(created_at,1,10)=?`
	args := []any{channel, date}
	if status != "" {
		q += " AND status=?"
		args = append(args, status)
	}
	var count, amount int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&count, &amount); err != nil {
		return 0, 0, err
	}
	return count, amount, nil
}

// OrdersTrend 按日期范围统计订单/金额趋势。start/end 为 "YYYY-MM-DD"；空则回退到近 90 天。
func (s *SQLiteStore) OrdersTrend(ctx context.Context, start, end string) ([]DailyStat, error) {
	// 解析范围：缺省回退近 90 天（订单表长期保留，趋势数据随时可查，仅图表默认范围受限）
	from, err := time.Parse("2006-01-02", start)
	if err != nil {
		from = time.Now().AddDate(0, 0, -89)
	}
	to, err := time.Parse("2006-01-02", end)
	if err != nil {
		to = time.Now()
	}
	if from.After(to) {
		from, to = to, from
	}
	// 上限防护：单次最多拉 370 天，避免超大范围拖垮图表
	if to.Sub(from) > 370*24*time.Hour {
		from = to.AddDate(0, 0, -369)
	}
	fromStr, toStr := from.Format("2006-01-02"), to.Format("2006-01-02")

	rows, err := s.db.QueryContext(ctx, `
		SELECT substr(created_at, 1, 10) AS d,
		       COUNT(*) AS total,
		       SUM(CASE WHEN status='TRADE_SUCCESS' THEN 1 ELSE 0 END) AS succ,
		       COALESCE(SUM(CASE WHEN status='TRADE_SUCCESS' THEN amount_minor ELSE 0 END), 0) AS amt
		FROM orders
		WHERE substr(created_at, 1, 10) >= ? AND substr(created_at, 1, 10) <= ?
		GROUP BY d ORDER BY d ASC`, fromStr, toStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 用 map 填充缺失日期
	byDate := make(map[string]DailyStat)
	for rows.Next() {
		var d DailyStat
		if err := rows.Scan(&d.Date, &d.TotalOrders, &d.SuccessOrders, &d.AmountMinor); err != nil {
			return nil, err
		}
		byDate[d.Date] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 填充完整日期序列（缺失的天补 0）
	out := make([]DailyStat, 0, int(to.Sub(from)/24/time.Hour)+1)
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		if v, ok := byDate[date]; ok {
			out = append(out, v)
		} else {
			out = append(out, DailyStat{Date: date})
		}
	}
	return out, nil
}

func (s *SQLiteStore) MarkNotified(ctx context.Context, outTradeNo string, notifyCount int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE orders SET notified=1, notify_count=?, notified_at=COALESCE(notified_at, ?) WHERE out_trade_no=?`,
		notifyCount, nowStr(), outTradeNo)
	return err
}

// ---------- 资金流水 ----------

func (s *SQLiteStore) InsertEvent(ctx context.Context, orderID int64, eventType, channelTxID string, amountMinor int64, rawPayload string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO payment_events (order_id, event_type, channel_tx_id, amount_minor, raw_payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		orderID, eventType, channelTxID, amountMinor, rawPayload, nowStr())
	return err
}

func (s *SQLiteStore) ListEvents(ctx context.Context, orderID int64, limit int) ([]*PaymentEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, order_id, event_type, channel_tx_id, amount_minor, raw_payload, created_at
		 FROM payment_events WHERE order_id=? ORDER BY id DESC LIMIT ?`, orderID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*PaymentEvent{} // 空数组而非 nil，避免 JSON 序列化为 null
	for rows.Next() {
		var e PaymentEvent
		var created string
		if err := rows.Scan(&e.ID, &e.OrderID, &e.EventType, &e.ChannelTxID, &e.AmountMinor, &e.RawPayload, &created); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ---------- 回调任务（outbox） ----------

func (s *SQLiteStore) CreateNotifyTask(ctx context.Context, orderID int64, payload string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO notify_tasks (order_id, payload, status, next_retry_at, created_at, updated_at)
		 VALUES (?, ?, 'pending', ?, ?, ?)`,
		orderID, payload, nowStr(), nowStr(), nowStr())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *SQLiteStore) GetPendingNotifyTasks(ctx context.Context, limit int) ([]*NotifyTask, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, order_id, payload, status, retry_count, next_retry_at, created_at, updated_at
		 FROM notify_tasks WHERE status='pending' AND next_retry_at <= ?
		 ORDER BY id ASC LIMIT ?`, nowStr(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

// GetNotifyTask 按 id 查询单个回调任务（死信重发定位用）
func (s *SQLiteStore) GetNotifyTask(ctx context.Context, id int64) (*NotifyTask, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, order_id, payload, status, retry_count, next_retry_at, created_at, updated_at
		 FROM notify_tasks WHERE id=?`, id)
	var t NotifyTask
	var nextRetry, created, updated string
	if err := row.Scan(&t.ID, &t.OrderID, &t.Payload, &t.Status, &t.RetryCount, &nextRetry, &created, &updated); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	t.NextRetryAt, _ = time.Parse(time.RFC3339Nano, nextRetry)
	t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return &t, nil
}

// UpdateNotifyTaskPayload 更新回调任务 payload（死信重发时按订单最新 notify_url 重建）
func (s *SQLiteStore) UpdateNotifyTaskPayload(ctx context.Context, id int64, payload string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE notify_tasks SET payload=?, updated_at=? WHERE id=?`, payload, nowStr(), id)
	return err
}

// DeleteNotifyTask 删除回调任务（彻底移除死信，确认放弃该笔通知）
func (s *SQLiteStore) DeleteNotifyTask(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM notify_tasks WHERE id=?`, id)
	return err
}

func (s *SQLiteStore) ListNotifyTasks(ctx context.Context, status string, limit int) ([]*NotifyTask, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, order_id, payload, status, retry_count, next_retry_at, created_at, updated_at
		 FROM notify_tasks WHERE (?='' OR status=?) ORDER BY id DESC LIMIT ?`,
		status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTasks(rows)
}

func (s *SQLiteStore) CountNotifyTasks(ctx context.Context, status string) (int64, error) {
	q := `SELECT COUNT(*) FROM notify_tasks`
	args := []any{}
	if status != "" {
		q += " WHERE status=?"
		args = append(args, status)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func scanTasks(rows *sql.Rows) ([]*NotifyTask, error) {
	out := []*NotifyTask{}
	for rows.Next() {
		var t NotifyTask
		var nextRetry, created, updated string
		if err := rows.Scan(&t.ID, &t.OrderID, &t.Payload, &t.Status, &t.RetryCount, &nextRetry, &created, &updated); err != nil {
			return nil, err
		}
		t.NextRetryAt, _ = time.Parse(time.RFC3339Nano, nextRetry)
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateNotifyTask(ctx context.Context, id int64, status string, retryCount int, nextRetryAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE notify_tasks SET status=?, retry_count=?, next_retry_at=?, updated_at=? WHERE id=?`,
		status, retryCount, nextRetryAt.Format(time.RFC3339Nano), nowStr(), id)
	return err
}

// ---------- 日志 ----------

func (s *SQLiteStore) InsertLog(ctx context.Context, level, scope, message, traceID, fields string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO app_logs (level, scope, message, trace_id, fields, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		level, scope, message, traceID, fields, nowStr())
	return err
}

func (s *SQLiteStore) QueryLogs(ctx context.Context, level, scope string, limit, offset int) ([]*LogRecord, error) {
	q := `SELECT id, level, scope, message, trace_id, fields, created_at FROM app_logs WHERE 1=1`
	args := []any{}
	if level != "" {
		q += " AND LOWER(level)=LOWER(?)"
		args = append(args, level)
	}
	if scope != "" {
		q += " AND scope=?"
		args = append(args, scope)
	}
	q += " ORDER BY id DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*LogRecord{}
	for rows.Next() {
		var l LogRecord
		var created string
		if err := rows.Scan(&l.ID, &l.Level, &l.Scope, &l.Message, &l.TraceID, &l.Fields, &created); err != nil {
			return nil, err
		}
		l.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &l)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) CountLogs(ctx context.Context, level, scope string) (int64, error) {
	q := `SELECT COUNT(*) FROM app_logs WHERE 1=1`
	args := []any{}
	if level != "" {
		q += " AND LOWER(level)=LOWER(?)"
		args = append(args, level)
	}
	if scope != "" {
		q += " AND scope=?"
		args = append(args, scope)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// ---------- 系统设置 ----------

func (s *SQLiteStore) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (s *SQLiteStore) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, nowStr())
	return err
}

func (s *SQLiteStore) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = ?`, key)
	return err
}

func (s *SQLiteStore) GetAllSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---------- 个人免签匹配查询 ----------

// orderColumns 常用 SELECT 列（与 scanOrder 对应）
const orderColumns = `id, out_trade_no, gateway_no, channel, channel_tx_id, account_id, pid, upstream_name, attach, amount_minor,
	currency, subject, status, notify_url, return_url, qr_code, pay_url, payer, notified, notify_count, created_at, session_expires_at, paid_at, notified_at,
	note_code, qr_token, device_id, buyer_key, fp_key`

// GetOrderByNoteCode 按备注码查询未支付订单（精确匹配，个人免签主匹配）
// since: 只查该时间点之后创建的订单（5 分钟窗口）
func (s *SQLiteStore) GetOrderByNoteCode(ctx context.Context, noteCode string, since time.Time) (*model.Order, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE note_code=? AND status=? AND created_at >= ? ORDER BY id LIMIT 1`,
		noteCode, model.StatusWaitPay, since.Format(time.RFC3339))
	return scanOrder(row)
}

// GetOrdersByAmount 查询某时间窗口内同金额的未支付订单（金额兜底匹配）
func (s *SQLiteStore) GetOrdersByAmount(ctx context.Context, amountMinor int64, since time.Time) ([]*model.Order, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE amount_minor=? AND status=? AND created_at >= ? ORDER BY id`,
		amountMinor, model.StatusWaitPay, since.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetRecentSettledByAmount 查询某时间窗口内同金额的已支付订单（重复捕获判定用）
func (s *SQLiteStore) GetRecentSettledByAmount(ctx context.Context, amountMinor int64, since time.Time, accountIDs []int64) ([]*model.Order, error) {
	query := `SELECT ` + orderColumns + ` FROM orders
		 WHERE amount_minor=? AND status=? AND created_at >= ?`
	args := []interface{}{amountMinor, model.StatusSuccess, since.Format(time.RFC3339)}
	// 按当前设备可服务账户集合限缩：同金额但属其他设备/账户的已支付订单不纳入，
	// 避免「通知栏已入账的那笔」被账单页误判为无订单收款。
	if len(accountIDs) > 0 {
		placeholders := make([]string, 0, len(accountIDs))
		for _, id := range accountIDs {
			placeholders = append(placeholders, "?")
			args = append(args, id)
		}
		query += ` AND account_id IN (` + strings.Join(placeholders, ",") + `)`
	}
	query += ` ORDER BY id DESC LIMIT 2`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetOrderByQrToken 按套码 token 查询订单（/mq/{token} 跳转校验）
func (s *SQLiteStore) GetOrderByQrToken(ctx context.Context, qrToken string) (*model.Order, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM orders WHERE qr_token=? ORDER BY id LIMIT 1`, qrToken)
	return scanOrder(row)
}

// ListWaitPayByAccount 查询指定收款账户下所有待支付订单
// 个人免签：监听端离线联动用（设备离线 → 其绑定账户的 WAIT_PAY 订单进异常池人工核实）
func (s *SQLiteStore) ListWaitPayByAccount(ctx context.Context, accountID int64) ([]*model.Order, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE account_id=? AND status=? ORDER BY id`,
		accountID, model.StatusWaitPay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListWaitPayAll 查询全部待支付订单（WaitPayPool 启动重建 + 周期对账的数据源）
func (s *SQLiteStore) ListWaitPayAll(ctx context.Context) ([]*model.Order, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE status=? ORDER BY id`,
		model.StatusWaitPay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListWaitPayByBuyer 查询指定买家环境(buyer_key)下所有待支付订单
// 买家级串行：一个买家环境同时只允许一笔待支付订单（防海量刷单/错配攻击）
func (s *SQLiteStore) ListWaitPayByBuyer(ctx context.Context, buyerKey string) ([]*model.Order, error) {
	if buyerKey == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE buyer_key=? AND status=? ORDER BY id`,
		buyerKey, model.StatusWaitPay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ListWaitPayByBuyerOrFP 按买家环境(buyer_key)或浏览器指纹(fp_key)查询待支付订单
// 支付页 JS 上报指纹后订单持有 fp_key；同环境换 IP 再下单时 buyer_key 不同，
// 但 fp_key 相同 → 串行检查需同时匹配两维（防指纹绑定后串行被绕过）
func (s *SQLiteStore) ListWaitPayByBuyerOrFP(ctx context.Context, buyerKey, fpKey string) ([]*model.Order, error) {
	if buyerKey == "" && fpKey == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders
		 WHERE status=? AND (buyer_key=? OR (fp_key != '' AND fp_key=?)) ORDER BY id`,
		model.StatusWaitPay, buyerKey, fpKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Close() error { return s.db.Close() }
