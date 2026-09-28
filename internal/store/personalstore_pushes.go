package store

import (
	"context"
	"database/sql"
	"time"

	"paygateway/internal/personal"
)

// ---------- 收款推送流水 ----------

// InsertPush 写入收款事件流水（审计）
func (p *PersonalDB) InsertPush(ctx context.Context, pu *personal.Push) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT INTO personal_pushes (device_id, provider, amount_minor, note, raw_hash, matched_order, match_result, risk_text, space, raw, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pu.DeviceID, pu.Provider, pu.AmountMinor, pu.Note, pu.RawHash,
		pu.MatchedOrder, pu.MatchResult, pu.RiskText, pu.Space, pu.Raw, nowStr())
	return err
}

// ListPushes 列出收款事件流水
func (p *PersonalDB) ListPushes(ctx context.Context, limit int) ([]*personal.Push, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, device_id, provider, amount_minor, note, raw_hash, matched_order, match_result, risk_text, space, raw, created_at
		 FROM personal_pushes ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*personal.Push
	for rows.Next() {
		var pu personal.Push
		var created string
		if err := rows.Scan(&pu.ID, &pu.DeviceID, &pu.Provider, &pu.AmountMinor, &pu.Note,
			&pu.RawHash, &pu.MatchedOrder, &pu.MatchResult, &pu.RiskText, &pu.Space, &pu.Raw, &created); err != nil {
			return nil, err
		}
		pu.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &pu)
	}
	if out == nil {
		out = []*personal.Push{}
	}
	return out, rows.Err()
}

// CountPushes 统计收款事件数
func (p *PersonalDB) CountPushes(ctx context.Context) (int64, error) {
	var n int64
	err := p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personal_pushes`).Scan(&n)
	return n, err
}

// PushExists 幂等去重：同设备同通知原文哈希是否已处理
func (p *PersonalDB) PushExists(ctx context.Context, deviceID, rawHash string) (bool, error) {
	if rawHash == "" {
		return false, nil
	}
	var n int64
	err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM personal_pushes WHERE device_id=? AND raw_hash=?`,
		deviceID, rawHash).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ---------- 无订单收款 ----------

// CreateUnmatched 创建无订单收款记录
func (p *PersonalDB) CreateUnmatched(ctx context.Context, u *personal.UnmatchedPayment) (int64, error) {
	res, err := p.db.ExecContext(ctx,
		`INSERT INTO unmatched_payments (provider, amount_minor, note, raw, status, handler, payer, device_id, risk_tags, risk_level, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Provider, u.AmountMinor, u.Note, u.Raw, u.Status, u.Handler, u.Payer, u.DeviceID, u.RiskTags, u.RiskLevel, nowStr())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// unmatchedCols 无订单收款查询列（与 ListUnmatched 扫描顺序一致）
var unmatchedCols = `id, provider, amount_minor, note, raw, status, handler, payer, device_id, risk_tags, risk_level, created_at`

// ListUnmatched 列出无订单收款
func (p *PersonalDB) ListUnmatched(ctx context.Context, status string, limit int) ([]*personal.UnmatchedPayment, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var rows *sql.Rows
	var err error
	if status == "" {
		rows, err = p.db.QueryContext(ctx,
			`SELECT `+unmatchedCols+` FROM unmatched_payments ORDER BY id DESC LIMIT ?`, limit)
	} else {
		rows, err = p.db.QueryContext(ctx,
			`SELECT `+unmatchedCols+` FROM unmatched_payments WHERE status=? ORDER BY id DESC LIMIT ?`, status, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*personal.UnmatchedPayment
	for rows.Next() {
		var u personal.UnmatchedPayment
		var created string
		if err := rows.Scan(&u.ID, &u.Provider, &u.AmountMinor, &u.Note, &u.Raw,
			&u.Status, &u.Handler, &u.Payer, &u.DeviceID, &u.RiskTags, &u.RiskLevel, &created); err != nil {
			return nil, err
		}
		u.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &u)
	}
	if out == nil {
		out = []*personal.UnmatchedPayment{}
	}
	return out, rows.Err()
}

// CountUnmatched 统计无订单收款数
func (p *PersonalDB) CountUnmatched(ctx context.Context, status string) (int64, error) {
	var n int64
	var err error
	if status == "" {
		err = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM unmatched_payments`).Scan(&n)
	} else {
		err = p.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM unmatched_payments WHERE status=?`, status).Scan(&n)
	}
	return n, err
}

// ResolveUnmatched 处理无订单收款
func (p *PersonalDB) ResolveUnmatched(ctx context.Context, id int64, status, handler string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE unmatched_payments SET status=?, handler=? WHERE id=?`,
		status, handler, id)
	return err
}
