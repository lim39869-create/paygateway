package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"paygateway/internal/model"
)

// ========== 人工异常处理池 ==========

const exceptionCols = `id, out_trade_no, pid, upstream_name, channel, amount_minor, order_status,
	channel_status, channel_tx_id, paid_amount, payer, reason, phase, resolved_action,
	evidence, suggestion, handler, handler_note, resolved_at, updated_at, created_at`

func scanException(row *sql.Row) (*model.ExceptionOrder, error) {
	var e model.ExceptionOrder
	var updatedAt, createdAt string
	err := row.Scan(&e.ID, &e.OutTradeNo, &e.Pid, &e.UpstreamName, &e.Channel, &e.AmountMinor,
		&e.OrderStatus, &e.ChannelStatus, &e.ChannelTxID, &e.PaidAmount, &e.Payer, &e.Reason,
		&e.Phase, &e.ResolvedAction, &e.Evidence, &e.Suggestion, &e.Handler, &e.HandlerNote,
		&e.ResolvedAt, &updatedAt, &createdAt)
	if err != nil {
		return nil, err
	}
	e.UpdatedAt = updatedAt
	e.CreatedAt = createdAt
	return &e, nil
}

// CreateException 创建异常单（幂等：同 out_trade_no 已存在则返回已有）
func (s *SQLiteStore) CreateException(ctx context.Context, e *model.ExceptionOrder) error {
	now := time.Now().Format(time.RFC3339)
	if e.PaidAmount == 0 {
		e.PaidAmount = e.AmountMinor
	}
	if e.Phase == "" {
		e.Phase = model.ExPhasePending
	}
	if e.ChannelStatus == "" {
		e.ChannelStatus = "UNKNOWN"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO exception_orders
		(out_trade_no, pid, upstream_name, channel, amount_minor, order_status, channel_status,
		 channel_tx_id, paid_amount, payer, reason, phase, resolved_action, evidence, suggestion,
		 handler, handler_note, resolved_at, updated_at, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(out_trade_no) DO NOTHING`,
		e.OutTradeNo, e.Pid, e.UpstreamName, e.Channel, e.AmountMinor, e.OrderStatus, e.ChannelStatus,
		e.ChannelTxID, e.PaidAmount, e.Payer, e.Reason, e.Phase, e.ResolvedAction, e.Evidence, e.Suggestion,
		e.Handler, e.HandlerNote, e.ResolvedAt, now, now)
	if err != nil {
		return fmt.Errorf("创建异常单失败: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetException(ctx context.Context, id int64) (*model.ExceptionOrder, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+exceptionCols+` FROM exception_orders WHERE id = ?`, id)
	return scanException(row)
}

func (s *SQLiteStore) GetExceptionByOutTradeNo(ctx context.Context, outTradeNo string) (*model.ExceptionOrder, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+exceptionCols+` FROM exception_orders WHERE out_trade_no = ?`, outTradeNo)
	return scanException(row)
}

func (s *SQLiteStore) ListExceptions(ctx context.Context, phase, reason string, limit, offset int) ([]*model.ExceptionOrder, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT ` + exceptionCols + ` FROM exception_orders WHERE 1=1`
	args := []any{}
	if phase != "" {
		q += ` AND phase = ?`
		args = append(args, phase)
	}
	if reason != "" {
		q += ` AND reason = ?`
		args = append(args, reason)
	}
	q += ` ORDER BY id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ExceptionOrder
	for rows.Next() {
		var e model.ExceptionOrder
		var updatedAt, createdAt string
		if err := rows.Scan(&e.ID, &e.OutTradeNo, &e.Pid, &e.UpstreamName, &e.Channel, &e.AmountMinor,
			&e.OrderStatus, &e.ChannelStatus, &e.ChannelTxID, &e.PaidAmount, &e.Payer, &e.Reason,
			&e.Phase, &e.ResolvedAction, &e.Evidence, &e.Suggestion, &e.Handler, &e.HandlerNote,
			&e.ResolvedAt, &updatedAt, &createdAt); err != nil {
			return nil, err
		}
		e.UpdatedAt = updatedAt
		e.CreatedAt = createdAt
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) CountExceptions(ctx context.Context, phase string) (int64, error) {
	q := `SELECT COUNT(*) FROM exception_orders`
	args := []any{}
	if phase != "" {
		q += ` WHERE phase = ?`
		args = append(args, phase)
	}
	var n int64
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// UpdateExceptionPhase 条件更新阶段（防并发处理：pending -> processing 锁）
func (s *SQLiteStore) UpdateExceptionPhase(ctx context.Context, id int64, from, to string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE exception_orders SET phase = ?, updated_at = ? WHERE id = ? AND phase = ?`,
		to, time.Now().Format(time.RFC3339), id, from)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// UpdateException 更新异常单（set 修改字段，updated_at 自动刷新）
func (s *SQLiteStore) UpdateException(ctx context.Context, id int64, set func(e *model.ExceptionOrder)) error {
	e, err := s.GetException(ctx, id)
	if err != nil {
		return err
	}
	set(e)
	_, err = s.db.ExecContext(ctx, `UPDATE exception_orders SET
		channel_status=?, channel_tx_id=?, paid_amount=?, payer=?, reason=?, phase=?,
		resolved_action=?, evidence=?, suggestion=?, handler=?, handler_note=?, resolved_at=?, updated_at=?
		WHERE id=?`,
		e.ChannelStatus, e.ChannelTxID, e.PaidAmount, e.Payer, e.Reason, e.Phase,
		e.ResolvedAction, e.Evidence, e.Suggestion, e.Handler, e.HandlerNote, e.ResolvedAt,
		time.Now().Format(time.RFC3339), id)
	return err
}

func (s *SQLiteStore) InsertExceptionLog(ctx context.Context, exceptionID int64, action, operator, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO exception_logs (exception_id, action, operator, detail, created_at)
		VALUES (?,?,?,?,?)`, exceptionID, action, operator, detail, time.Now().Format(time.RFC3339))
	return err
}

func (s *SQLiteStore) ListExceptionLogs(ctx context.Context, exceptionID int64, limit int) ([]*model.ExceptionLog, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, exception_id, action, operator, detail, created_at
		FROM exception_logs WHERE exception_id = ? ORDER BY id DESC LIMIT ?`, exceptionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.ExceptionLog
	for rows.Next() {
		var l model.ExceptionLog
		if err := rows.Scan(&l.ID, &l.ExceptionID, &l.Action, &l.Operator, &l.Detail, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &l)
	}
	return out, rows.Err()
}

// UpdateOrderHandled 订单人工处理标记（人工池处理后永久留痕）
func (s *SQLiteStore) UpdateOrderHandled(ctx context.Context, outTradeNo string, exceptionID int64, action, handler, note string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE orders SET
		exception_id = ?, handled_by = ?, handled_action = ?, handled_note = ?, handled_at = ?
		WHERE out_trade_no = ?`,
		exceptionID, handler, action, note, time.Now().Format(time.RFC3339), outTradeNo)
	return err
}
