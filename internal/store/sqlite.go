package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"paygateway/internal/model"

	_ "modernc.org/sqlite"
)

// SQLiteStore SQLite 存储实现
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLite 打开 SQLite 数据库并执行迁移
func NewSQLite(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// 关键配置：WAL 模式 + busy timeout（并发安全）
	db.SetMaxOpenConns(1) // SQLite 单写者，串行化保证一致性
	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		return nil, fmt.Errorf("设置 WAL 失败: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		return nil, fmt.Errorf("设置 busy_timeout 失败: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON;"); err != nil {
		return nil, fmt.Errorf("设置外键失败: %w", err)
	}
	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate 建表（幂等）
func (s *SQLiteStore) migrate() error {
	// 迁移旧数据库：添加 account_id / pid / upstream_name 列（如果不存在）
	_, _ = s.db.Exec(`ALTER TABLE orders ADD COLUMN account_id INTEGER NOT NULL DEFAULT 0`)
	_, _ = s.db.Exec(`ALTER TABLE orders ADD COLUMN pid TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE orders ADD COLUMN upstream_name TEXT NOT NULL DEFAULT ''`)
	// 迁移旧数据库：个人免签通道字段（列已存在时 ALTER 报错被忽略，新库直接走 CREATE TABLE）
	_, _ = s.db.Exec(`ALTER TABLE orders ADD COLUMN note_code TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE orders ADD COLUMN qr_token TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE orders ADD COLUMN device_id TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE orders ADD COLUMN buyer_key TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_orders_buyer_key ON orders(buyer_key) WHERE buyer_key != ''`)
	_, _ = s.db.Exec(`ALTER TABLE orders ADD COLUMN fp_key TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`ALTER TABLE orders ADD COLUMN session_expires_at TEXT NOT NULL DEFAULT ''`)
	_, _ = s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_orders_fp_key ON orders(fp_key) WHERE fp_key != ''`)
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS orders (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			out_trade_no    TEXT NOT NULL UNIQUE,
			gateway_no      TEXT NOT NULL UNIQUE,
			channel         TEXT NOT NULL,
			channel_tx_id   TEXT DEFAULT NULL,
			account_id      INTEGER NOT NULL DEFAULT 0,
			pid             TEXT NOT NULL DEFAULT '',
			upstream_name   TEXT NOT NULL DEFAULT '',
			attach          TEXT NOT NULL DEFAULT '',
			amount_minor    INTEGER NOT NULL,
			currency        TEXT NOT NULL DEFAULT 'CNY',
			subject         TEXT NOT NULL,
			status          TEXT NOT NULL,
			notify_url      TEXT,
			return_url      TEXT,
			qr_code         TEXT,
			pay_url         TEXT,
			payer           TEXT,
			notified        INTEGER NOT NULL DEFAULT 0,
			notify_count    INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL,
			session_expires_at TEXT NOT NULL DEFAULT '',
			paid_at         TEXT,
			notified_at     TEXT,
			note_code       TEXT NOT NULL DEFAULT '',
			qr_token        TEXT NOT NULL DEFAULT '',
			device_id       TEXT NOT NULL DEFAULT '',
			buyer_key       TEXT NOT NULL DEFAULT '',
			fp_key          TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_orders_channel_tx ON orders(channel, channel_tx_id) WHERE channel_tx_id IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_created ON orders(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_channel ON orders(channel)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_pid ON orders(pid)`,
		`CREATE INDEX IF NOT EXISTS idx_orders_note_code ON orders(note_code) WHERE note_code != ''`,
		`CREATE INDEX IF NOT EXISTS idx_orders_qr_token ON orders(qr_token) WHERE qr_token != ''`,
		// 调度器/池按账户查待支付订单（ListWaitPayByAccount / ListWaitPayAll 高频路径）
		`CREATE INDEX IF NOT EXISTS idx_orders_account_status ON orders(account_id, status)`,
		`CREATE TABLE IF NOT EXISTS payment_events (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id        INTEGER NOT NULL,
			event_type      TEXT NOT NULL,
			channel_tx_id   TEXT,
			amount_minor    INTEGER NOT NULL,
			raw_payload     TEXT,
			created_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_order ON payment_events(order_id)`,
		`CREATE TABLE IF NOT EXISTS notify_tasks (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			order_id        INTEGER NOT NULL,
			payload         TEXT NOT NULL,
			status          TEXT NOT NULL DEFAULT 'pending',
			retry_count     INTEGER NOT NULL DEFAULT 0,
			next_retry_at   TEXT NOT NULL,
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notify_status ON notify_tasks(status, next_retry_at)`,
		`CREATE TABLE IF NOT EXISTS callback_logs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id     INTEGER NOT NULL,
			http_status INTEGER,
			response    TEXT,
			latency_ms  INTEGER,
			created_at  TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS exception_orders (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			out_trade_no    TEXT NOT NULL UNIQUE,
			pid             TEXT NOT NULL DEFAULT '',
			upstream_name   TEXT NOT NULL DEFAULT '',
			channel         TEXT NOT NULL DEFAULT '',
			amount_minor    INTEGER NOT NULL DEFAULT 0,
			order_status    TEXT NOT NULL DEFAULT '',
			channel_status  TEXT NOT NULL DEFAULT 'UNKNOWN',
			channel_tx_id   TEXT NOT NULL DEFAULT '',
			paid_amount     INTEGER NOT NULL DEFAULT 0,
			payer           TEXT NOT NULL DEFAULT '',
			reason          TEXT NOT NULL DEFAULT '',
			phase           TEXT NOT NULL DEFAULT 'pending',
			resolved_action TEXT NOT NULL DEFAULT '',
			evidence        TEXT NOT NULL DEFAULT '',
			suggestion      TEXT NOT NULL DEFAULT '',
			handler         TEXT NOT NULL DEFAULT '',
			handler_note    TEXT NOT NULL DEFAULT '',
			resolved_at     TEXT NOT NULL DEFAULT '',
			updated_at      TEXT NOT NULL,
			created_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_exception_phase ON exception_orders(phase, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_exception_reason ON exception_orders(reason)`,
		`CREATE TABLE IF NOT EXISTS exception_logs (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			exception_id INTEGER NOT NULL,
			action       TEXT NOT NULL,
			operator     TEXT NOT NULL DEFAULT '',
			detail       TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_exception_logs ON exception_logs(exception_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS app_logs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			level       TEXT NOT NULL,
			scope       TEXT,
			message     TEXT NOT NULL,
			trace_id    TEXT,
			fields      TEXT,
			created_at  TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_level_time ON app_logs(level, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_scope_time ON app_logs(scope, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_logs_trace ON app_logs(trace_id)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key         TEXT PRIMARY KEY,
			value       TEXT NOT NULL DEFAULT '',
			updated_at  TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("迁移失败: %w", err)
		}
	}
	return nil
}

func nowStr() string { return time.Now().Format(time.RFC3339Nano) }

// ---------- 订单 ----------

func (s *SQLiteStore) CreateOrder(ctx context.Context, o *model.Order) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO orders (out_trade_no, gateway_no, channel, channel_tx_id, account_id, pid, upstream_name, attach,
			amount_minor, currency, subject, status, notify_url, return_url, qr_code, pay_url, payer, notified, notify_count, created_at, session_expires_at,
			note_code, qr_token, device_id, buyer_key, fp_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		o.OutTradeNo, o.GatewayNo, o.Channel, nullIfEmpty(o.ChannelTxID), o.AccountID, o.Pid, o.UpstreamName, o.Attach,
		o.AmountMinor, o.Currency, o.Subject, o.Status, o.NotifyURL, o.ReturnURL, o.QrCode, o.PayURL, o.Payer,
		o.Notified, o.NotifyCount, o.CreatedAt.Format(time.RFC3339Nano), nullIfTimeValue(o.SessionExpiresAt),
		o.NoteCode, o.QrToken, o.DeviceID, o.BuyerKey, o.FPKey)
	return err
}

func nullIfTimeValue(t time.Time) any {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullIfTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(time.RFC3339Nano)
}

// ReplaceOrder 原子完成买家替换：同一事务内把 WAIT_PAY 订单改为 CLOSED 并写 replaced 事件。
// replaced 事件与关单同事务持久化 ⇒ 观察者看到 CLOSED 时必能看到 replaced 事件，
// 从而可靠区分「并发替换者关闭」（有 replaced）与「清扫器超时关闭」（无 replaced）。
// 返回 changed=true 表示本次调用赢得替换；false 表示订单已非 WAIT_PAY（被并发结算或替换者处理）。
func (s *SQLiteStore) ReplaceOrder(ctx context.Context, outTradeNo string, orderID int64, amountMinor int64, rawJSON string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE orders SET status=?, paid_at=COALESCE(paid_at, ?) WHERE out_trade_no=? AND status=?`,
		model.StatusClosed, nowStr(), outTradeNo, model.StatusWaitPay)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// 非 WAIT_PAY（已结算 / 已被并发替换关闭）→ 未赢得替换
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO payment_events (order_id, event_type, channel_tx_id, amount_minor, raw_payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		orderID, "replaced", "", amountMinor, rawJSON, nowStr()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// UpdateOrderAmount 更新订单金额（复用订单时以最新下单金额覆盖）。
func (s *SQLiteStore) UpdateOrderAmount(ctx context.Context, outTradeNo string, amountMinor int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE orders SET amount_minor=? WHERE out_trade_no=? AND amount_minor<>?`,
		amountMinor, outTradeNo, amountMinor)
	return err
}

func (s *SQLiteStore) GetOrder(ctx context.Context, outTradeNo string) (*model.Order, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, out_trade_no, gateway_no, channel, channel_tx_id, account_id, pid, upstream_name, attach, amount_minor,
		currency, subject, status, notify_url, return_url, qr_code, pay_url, payer, notified, notify_count, created_at, session_expires_at, paid_at, notified_at,
		note_code, qr_token, device_id, buyer_key, fp_key
		FROM orders WHERE out_trade_no = ?`, outTradeNo)
	return scanOrder(row)
}

func (s *SQLiteStore) DeleteOrder(ctx context.Context, outTradeNo string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM orders WHERE out_trade_no=? AND status=?`,
		outTradeNo, model.StatusWaitPay)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *SQLiteStore) GetOrderByID(ctx context.Context, id int64) (*model.Order, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, out_trade_no, gateway_no, channel, channel_tx_id, account_id, pid, upstream_name, attach, amount_minor,
		currency, subject, status, notify_url, return_url, qr_code, pay_url, payer, notified, notify_count, created_at, session_expires_at, paid_at, notified_at,
		note_code, qr_token, device_id, buyer_key, fp_key
		FROM orders WHERE id = ?`, id)
	return scanOrder(row)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOrder(row rowScanner) (*model.Order, error) {
	var (
		o                           model.Order
		notified                    int
		createdAt, sessionExpiresAt string
		paidAt, nb, txID            sql.NullString
	)
	if err := row.Scan(&o.ID, &o.OutTradeNo, &o.GatewayNo, &o.Channel, &txID, &o.AccountID, &o.Pid, &o.UpstreamName, &o.Attach, &o.AmountMinor,
		&o.Currency, &o.Subject, &o.Status, &o.NotifyURL, &o.ReturnURL, &o.QrCode, &o.PayURL, &o.Payer,
		&notified, &o.NotifyCount, &createdAt, &sessionExpiresAt, &paidAt, &nb,
		&o.NoteCode, &o.QrToken, &o.DeviceID, &o.BuyerKey, &o.FPKey); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	o.Notified = notified != 0
	o.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	o.SessionExpiresAt, _ = time.Parse(time.RFC3339Nano, sessionExpiresAt)
	if paidAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, paidAt.String)
		o.PaidAt = &t
	}
	if nb.Valid {
		t, _ := time.Parse(time.RFC3339Nano, nb.String)
		o.NotifiedAt = &t
	}
	if txID.Valid {
		o.ChannelTxID = txID.String
	}
	return &o, nil
}

// UpdateOrderStatus 条件更新：from -> to；影响行数=0 表示状态非法（幂等/防覆盖）
// set 回调可修改订单字段，修改后自动写回数据库
func (s *SQLiteStore) UpdateOrderStatus(ctx context.Context, outTradeNo, from, to string, set func(o *model.Order)) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var updateSQL string
	var updateArgs []any
	if from == to {
		// 状态不变（如保存支付凭据），不修改 paid_at
		updateSQL = `UPDATE orders SET status=? WHERE out_trade_no=? AND status=?`
		updateArgs = []any{to, outTradeNo, from}
	} else {
		updateSQL = `UPDATE orders SET status=?, paid_at=COALESCE(paid_at, ?) WHERE out_trade_no=? AND status=?`
		updateArgs = []any{to, nowStr(), outTradeNo, from}
	}
	res, err := tx.ExecContext(ctx, updateSQL, updateArgs...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if set != nil {
		// 读回最新（在事务内用单独的查询）
		row := tx.QueryRowContext(ctx, `SELECT id, out_trade_no, gateway_no, channel, channel_tx_id, account_id, pid, upstream_name, attach, amount_minor,
			currency, subject, status, notify_url, return_url, qr_code, pay_url, payer, notified, notify_count, created_at, session_expires_at, paid_at, notified_at,
			note_code, qr_token, device_id, buyer_key, fp_key
			FROM orders WHERE out_trade_no = ?`, outTradeNo)
		upd, err := scanOrder(row)
		if err != nil {
			return false, err
		}
		if upd != nil {
			set(upd)
			// 写回修改的字段（含 channel/created_at/paid_at：订单换通道迁移/重置倒计时/复活清除支付时间用）
			_, err := tx.ExecContext(ctx,
				`UPDATE orders SET pay_url=?, qr_code=?, channel_tx_id=?, payer=?, account_id=?, notified=?, notify_count=?, notified_at=COALESCE(notified_at, ?),
				 note_code=?, qr_token=?, device_id=?, buyer_key=?, fp_key=?, channel=?, created_at=?, session_expires_at=?, paid_at=? WHERE out_trade_no=?`,
				upd.PayURL, upd.QrCode, nullIfEmpty(upd.ChannelTxID), upd.Payer, upd.AccountID, upd.Notified, upd.NotifyCount, nowStr(),
				upd.NoteCode, upd.QrToken, upd.DeviceID, upd.BuyerKey, upd.FPKey, upd.Channel, upd.CreatedAt.Format(time.RFC3339Nano), nullIfTimeValue(upd.SessionExpiresAt), nullIfTime(upd.PaidAt), outTradeNo)
			if err != nil {
				return false, err
			}
		}
	}
	return true, tx.Commit()
}
