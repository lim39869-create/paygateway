// Package store 账户数据库：独立 accounts.db，仅管理后台 API 可读写
//
// 安全设计：
//   - 独立 SQLite 文件，权限 0600
//   - 敏感字段（private_key / public_key / api_key）以 AES-256-GCM 密文存储
//   - AccountStore 接口不暴露给 order/channel 等模块，仅在 admin 包内部使用
//   - 其他模块通过 AccountPool（内存缓存）获取解密后的账户数据
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"paygateway/internal/account"
)

// AccountStore 账户存储接口（仅限 Admin API 使用）
type AccountStore interface {
	ListAccounts(ctx context.Context, channel string) ([]*account.AccountEncrypted, error)
	GetAccount(ctx context.Context, id int64) (*account.AccountEncrypted, error)
	CreateAccount(ctx context.Context, a *account.AccountEncrypted) (int64, error)
	UpdateAccount(ctx context.Context, a *account.AccountEncrypted) error
	DeleteAccount(ctx context.Context, id int64) error
	SetAccountStatus(ctx context.Context, id int64, status string) error
	ResetDailyUsed(ctx context.Context) error
	InsertAccountLog(ctx context.Context, log *account.AccountLog) error
	ListAccountLogs(ctx context.Context, accountID int64, limit int) ([]*account.AccountLog, error)
	Close() error
}

// AccountDB 独立账户数据库：仅存 pay_accounts + account_logs
// 文件权限 0600，WAL 模式，仅 Admin API 访问
type AccountDB struct {
	db *sql.DB
}

// NewAccountDB 打开/创建独立账户数据库
func NewAccountDB(path string) (*AccountDB, error) {
	// 先创建空文件以确保可以设置权限
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建账户数据库文件失败: %w", err)
	}
	fd.Close()

	// 强制设置权限为 0600（只有文件所有者可读写）
	if err := os.Chmod(path, 0600); err != nil {
		return nil, fmt.Errorf("设置账户数据库权限失败: %w", err)
	}

	// 打开连接：WAL 模式防崩溃
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=FULL")
	if err != nil {
		return nil, fmt.Errorf("打开账户数据库失败: %w", err)
	}

	// 连接池：单连接更安全
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	a := &AccountDB{db: db}
	if err := a.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

// migrate 创建账户表 + 操作审计日志表
func (a *AccountDB) migrate() error {
	// 迁移旧库：daily_used / qr_content_enc / frozen_reason 列（幂等，已存在则报错忽略）
	_, _ = a.db.Exec(`ALTER TABLE pay_accounts ADD COLUMN daily_used INTEGER NOT NULL DEFAULT 0`)
	_, _ = a.db.Exec(`ALTER TABLE pay_accounts ADD COLUMN qr_content_enc TEXT NOT NULL DEFAULT ''`)
	_, _ = a.db.Exec(`ALTER TABLE pay_accounts ADD COLUMN frozen_reason TEXT NOT NULL DEFAULT ''`)
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS pay_accounts (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			channel         TEXT NOT NULL,
			name            TEXT NOT NULL,
			app_id          TEXT NOT NULL DEFAULT '',
			mch_id          TEXT NOT NULL DEFAULT '',
			private_key_enc TEXT NOT NULL DEFAULT '',
			public_key_enc  TEXT NOT NULL DEFAULT '',
			api_key_enc     TEXT NOT NULL DEFAULT '',
			qr_content_enc  TEXT NOT NULL DEFAULT '',
			weight          INTEGER NOT NULL DEFAULT 50,
			priority        INTEGER NOT NULL DEFAULT 0,
			status          TEXT NOT NULL DEFAULT 'normal',
			frozen_reason   TEXT NOT NULL DEFAULT '',
			auto_ban        INTEGER NOT NULL DEFAULT 1,
			daily_limit     INTEGER NOT NULL DEFAULT 0,
			daily_used      INTEGER NOT NULL DEFAULT 0,
			group_name      TEXT NOT NULL DEFAULT 'default',
			is_prod         INTEGER NOT NULL DEFAULT 0,
			remark          TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_channel ON pay_accounts(channel)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_status ON pay_accounts(status)`,

		// 账户操作审计日志
		`CREATE TABLE IF NOT EXISTS account_logs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id  INTEGER NOT NULL,
			action      TEXT NOT NULL,
			detail      TEXT NOT NULL DEFAULT '',
			operator    TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_logs_account ON account_logs(account_id)`,

		// WAL 模式
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
	}
	for _, stmt := range stmts {
		if _, err := a.db.Exec(stmt); err != nil {
			return fmt.Errorf("账户数据库迁移失败: %w", err)
		}
	}
	return nil
}

// ---------- 账户 CRUD ----------

// ListAccounts 列出账户（按通道筛选，空=全部）
func (a *AccountDB) ListAccounts(ctx context.Context, channel string) ([]*account.AccountEncrypted, error) {
	var rows *sql.Rows
	var err error
	if channel == "" {
		rows, err = a.db.QueryContext(ctx,
			`SELECT id, channel, name, app_id, mch_id, private_key_enc, public_key_enc, api_key_enc, qr_content_enc,
			 weight, priority, status, frozen_reason, auto_ban, daily_limit, daily_used, group_name, is_prod, remark, created_at, updated_at
			 FROM pay_accounts ORDER BY channel, priority DESC, weight DESC`)
	} else {
		rows, err = a.db.QueryContext(ctx,
			`SELECT id, channel, name, app_id, mch_id, private_key_enc, public_key_enc, api_key_enc, qr_content_enc,
			 weight, priority, status, frozen_reason, auto_ban, daily_limit, daily_used, group_name, is_prod, remark, created_at, updated_at
			 FROM pay_accounts WHERE channel=? ORDER BY priority DESC, weight DESC`, channel)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*account.AccountEncrypted
	for rows.Next() {
		e, err := scanAccountEncrypted(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetAccount 按 ID 获取账户
func (a *AccountDB) GetAccount(ctx context.Context, id int64) (*account.AccountEncrypted, error) {
	row := a.db.QueryRowContext(ctx,
		`SELECT id, channel, name, app_id, mch_id, private_key_enc, public_key_enc, api_key_enc, qr_content_enc,
		 weight, priority, status, frozen_reason, auto_ban, daily_limit, daily_used, group_name, is_prod, remark, created_at, updated_at
		 FROM pay_accounts WHERE id=?`, id)
	return scanAccountEncrypted(row)
}

// CreateAccount 创建账户，返回新 ID
func (a *AccountDB) CreateAccount(ctx context.Context, acct *account.AccountEncrypted) (int64, error) {
	now := time.Now()
	res, err := a.db.ExecContext(ctx,
		`INSERT INTO pay_accounts (channel, name, app_id, mch_id, private_key_enc, public_key_enc, api_key_enc, qr_content_enc,
		 weight, priority, status, frozen_reason, auto_ban, daily_limit, daily_used, group_name, is_prod, remark, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		acct.Channel, acct.Name, acct.AppID, acct.MchID,
		acct.PrivateKeyEnc, acct.PublicKeyEnc, acct.ApiKeyEnc, acct.QRContentEnc,
		acct.Weight, acct.Priority, acct.Status, acct.FrozenReason, boolToInt(acct.AutoBan),
		acct.DailyLimit, acct.DailyUsed, acct.Group, boolToInt(acct.IsProd), acct.Remark,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("创建账户失败: %w", err)
	}
	return res.LastInsertId()
}

// UpdateAccount 更新账户（全部字段覆盖）
func (a *AccountDB) UpdateAccount(ctx context.Context, acct *account.AccountEncrypted) error {
	_, err := a.db.ExecContext(ctx,
		`UPDATE pay_accounts SET channel=?, name=?, app_id=?, mch_id=?,
		 private_key_enc=?, public_key_enc=?, api_key_enc=?, qr_content_enc=?,
		 weight=?, priority=?, status=?, frozen_reason=?, auto_ban=?, daily_limit=?, daily_used=?, group_name=?, is_prod=?, remark=?, updated_at=?
		 WHERE id=?`,
		acct.Channel, acct.Name, acct.AppID, acct.MchID,
		acct.PrivateKeyEnc, acct.PublicKeyEnc, acct.ApiKeyEnc, acct.QRContentEnc,
		acct.Weight, acct.Priority, acct.Status, acct.FrozenReason, boolToInt(acct.AutoBan),
		acct.DailyLimit, acct.DailyUsed, acct.Group, boolToInt(acct.IsProd), acct.Remark,
		time.Now().Format(time.RFC3339Nano), acct.ID)
	return err
}

// DeleteAccount 删除账户
func (a *AccountDB) DeleteAccount(ctx context.Context, id int64) error {
	_, err := a.db.ExecContext(ctx, `DELETE FROM pay_accounts WHERE id=?`, id)
	return err
}

// SetAccountStatus 设置账户状态（启停/冻结/解冻；解冻/启停时清空 frozen_reason）
func (a *AccountDB) SetAccountStatus(ctx context.Context, id int64, status string) error {
	_, err := a.db.ExecContext(ctx,
		`UPDATE pay_accounts SET status=?, frozen_reason=CASE WHEN ?='frozen' THEN frozen_reason ELSE '' END, updated_at=? WHERE id=?`,
		status, status, time.Now().Format(time.RFC3339Nano), id)
	return err
}

// SetAccountFrozen 冻结账户并记录原因（仅人工冻结路径使用；解冻统一走 SetAccountStatus）
func (a *AccountDB) SetAccountFrozen(ctx context.Context, id int64, reason string) error {
	_, err := a.db.ExecContext(ctx,
		`UPDATE pay_accounts SET status='frozen', frozen_reason=?, updated_at=? WHERE id=?`,
		reason, time.Now().Format(time.RFC3339Nano), id)
	return err
}

// ResetDailyUsed 重置所有账户的日已用额度（每日 00:00 调用）
func (a *AccountDB) ResetDailyUsed(ctx context.Context) error {
	_, err := a.db.ExecContext(ctx,
		`UPDATE pay_accounts SET daily_used=0, updated_at=?`,
		time.Now().Format(time.RFC3339Nano))
	return err
}

// ---------- 账户操作审计日志 ----------

// InsertAccountLog 写入操作审计日志
func (a *AccountDB) InsertAccountLog(ctx context.Context, log *account.AccountLog) error {
	_, err := a.db.ExecContext(ctx,
		`INSERT INTO account_logs (account_id, action, detail, operator, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		log.AccountID, log.Action, log.Detail, log.Operator, time.Now().Format(time.RFC3339Nano))
	return err
}

// ListAccountLogs 列出账户操作日志
func (a *AccountDB) ListAccountLogs(ctx context.Context, accountID int64, limit int) ([]*account.AccountLog, error) {
	rows, err := a.db.QueryContext(ctx,
		`SELECT id, account_id, action, detail, operator, created_at
		 FROM account_logs WHERE account_id=? ORDER BY id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*account.AccountLog, 0)
	for rows.Next() {
		var l account.AccountLog
		var created string
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Action, &l.Detail, &l.Operator, &created); err != nil {
			return nil, err
		}
		l.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &l)
	}
	return out, rows.Err()
}

// Close 关闭账户数据库
func (a *AccountDB) Close() error {
	return a.db.Close()
}

// ---------- 辅助函数 ----------

// scanAccountEncrypted 扫描一行到 AccountEncrypted
func scanAccountEncrypted(s rowScanner) (*account.AccountEncrypted, error) {
	var e account.AccountEncrypted
	var autoBan, isProd int
	var createdAt, updatedAt string
	err := s.Scan(&e.ID, &e.Channel, &e.Name, &e.AppID, &e.MchID,
		&e.PrivateKeyEnc, &e.PublicKeyEnc, &e.ApiKeyEnc, &e.QRContentEnc,
		&e.Weight, &e.Priority, &e.Status, &e.FrozenReason, &autoBan,
		&e.DailyLimit, &e.DailyUsed, &e.Group, &isProd, &e.Remark, &createdAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	e.AutoBan = autoBan != 0
	e.IsProd = isProd != 0
	e.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	e.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return &e, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
