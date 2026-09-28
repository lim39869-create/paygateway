// Package store 上游数据库：独立 upstreams.db，仅管理后台 API 可读写
//
// 安全设计：
//   - 独立 SQLite 文件，权限 0600
//   - 敏感字段（易支付密钥 key）以 AES-256-GCM 密文存储
//   - UpstreamStore 接口不暴露给 order/epay 等模块，仅在 admin 包内部使用
//   - 其他模块通过 UpstreamPool（内存缓存）获取解密后的上游数据
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"

	"paygateway/internal/upstream"
)

// UpstreamStore 上游存储接口（仅限 Admin API 使用）
type UpstreamStore interface {
	ListUpstreams(ctx context.Context) ([]*upstream.UpstreamEncrypted, error)
	GetUpstream(ctx context.Context, id int64) (*upstream.UpstreamEncrypted, error)
	CreateUpstream(ctx context.Context, u *upstream.UpstreamEncrypted) (int64, error)
	UpdateUpstream(ctx context.Context, u *upstream.UpstreamEncrypted) error
	DeleteUpstream(ctx context.Context, id int64) error
	SetUpstreamStatus(ctx context.Context, id int64, status string) error
	InsertUpstreamLog(ctx context.Context, log *upstream.UpstreamLog) error
	ListUpstreamLogs(ctx context.Context, upstreamID int64, limit int) ([]*upstream.UpstreamLog, error)
	Close() error
}

// UpstreamDB 独立上游数据库：仅存 upstreams + upstream_logs
// 文件权限 0600，WAL 模式，仅 Admin API 访问
type UpstreamDB struct {
	db *sql.DB
}

// NewUpstreamDB 打开/创建独立上游数据库
func NewUpstreamDB(path string) (*UpstreamDB, error) {
	// 先创建空文件以确保可以设置权限
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建上游数据库文件失败: %w", err)
	}
	fd.Close()

	// 强制设置权限为 0600（只有文件所有者可读写）
	if err := os.Chmod(path, 0600); err != nil {
		return nil, fmt.Errorf("设置上游数据库权限失败: %w", err)
	}

	// 打开连接：WAL 模式防崩溃
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=FULL")
	if err != nil {
		return nil, fmt.Errorf("打开上游数据库失败: %w", err)
	}

	// 连接池：单连接更安全
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	u := &UpstreamDB{db: db}
	if err := u.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return u, nil
}

// migrate 创建上游表 + 操作审计日志表
func (u *UpstreamDB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS upstreams (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			name            TEXT NOT NULL,
			pid             TEXT NOT NULL UNIQUE,
			key_enc         TEXT NOT NULL DEFAULT '',
			base_url        TEXT NOT NULL DEFAULT '',
			notify_domains  TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'enabled',
			remark          TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL,
			updated_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_upstreams_status ON upstreams(status)`,

		// 上游操作审计日志
		`CREATE TABLE IF NOT EXISTS upstream_logs (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			upstream_id  INTEGER NOT NULL,
			action       TEXT NOT NULL,
			detail       TEXT NOT NULL DEFAULT '',
			operator     TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_upstream_logs_upstream ON upstream_logs(upstream_id)`,

		// WAL 模式
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
	}
	for _, stmt := range stmts {
		if _, err := u.db.Exec(stmt); err != nil {
			return fmt.Errorf("上游数据库迁移失败: %w", err)
		}
	}
	return nil
}

// ---------- 上游 CRUD ----------

// ListUpstreams 列出全部上游
func (u *UpstreamDB) ListUpstreams(ctx context.Context) ([]*upstream.UpstreamEncrypted, error) {
	rows, err := u.db.QueryContext(ctx,
		`SELECT id, name, pid, key_enc, base_url, notify_domains, status, remark, created_at, updated_at
		 FROM upstreams ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*upstream.UpstreamEncrypted
	for rows.Next() {
		e, err := scanUpstreamEncrypted(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetUpstream 按 ID 获取上游
func (u *UpstreamDB) GetUpstream(ctx context.Context, id int64) (*upstream.UpstreamEncrypted, error) {
	row := u.db.QueryRowContext(ctx,
		`SELECT id, name, pid, key_enc, base_url, notify_domains, status, remark, created_at, updated_at
		 FROM upstreams WHERE id=?`, id)
	return scanUpstreamEncrypted(row)
}

// GetUpstreamByPid 按 pid 获取上游（启动迁移 default 用）
func (u *UpstreamDB) GetUpstreamByPid(ctx context.Context, pid string) (*upstream.UpstreamEncrypted, error) {
	row := u.db.QueryRowContext(ctx,
		`SELECT id, name, pid, key_enc, base_url, notify_domains, status, remark, created_at, updated_at
		 FROM upstreams WHERE pid=?`, pid)
	return scanUpstreamEncrypted(row)
}

// CreateUpstream 创建上游，返回新 ID
func (u *UpstreamDB) CreateUpstream(ctx context.Context, up *upstream.UpstreamEncrypted) (int64, error) {
	now := time.Now()
	res, err := u.db.ExecContext(ctx,
		`INSERT INTO upstreams (name, pid, key_enc, base_url, notify_domains, status, remark, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		up.Name, up.Pid, up.KeyEnc, up.BaseURL, up.NotifyDomains, up.Status, up.Remark,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("创建上游失败: %w", err)
	}
	return res.LastInsertId()
}

// UpdateUpstream 更新上游（全部字段覆盖）
func (u *UpstreamDB) UpdateUpstream(ctx context.Context, up *upstream.UpstreamEncrypted) error {
	_, err := u.db.ExecContext(ctx,
		`UPDATE upstreams SET name=?, pid=?, key_enc=?, base_url=?, notify_domains=?, status=?, remark=?, updated_at=?
		 WHERE id=?`,
		up.Name, up.Pid, up.KeyEnc, up.BaseURL, up.NotifyDomains, up.Status, up.Remark,
		time.Now().Format(time.RFC3339Nano), up.ID)
	return err
}

// DeleteUpstream 删除上游
func (u *UpstreamDB) DeleteUpstream(ctx context.Context, id int64) error {
	_, err := u.db.ExecContext(ctx, `DELETE FROM upstreams WHERE id=?`, id)
	return err
}

// SetUpstreamStatus 设置上游状态（启用/禁用）
func (u *UpstreamDB) SetUpstreamStatus(ctx context.Context, id int64, status string) error {
	_, err := u.db.ExecContext(ctx,
		`UPDATE upstreams SET status=?, updated_at=? WHERE id=?`,
		status, time.Now().Format(time.RFC3339Nano), id)
	return err
}

// ---------- 上游操作审计日志 ----------

// InsertUpstreamLog 写入操作审计日志
func (u *UpstreamDB) InsertUpstreamLog(ctx context.Context, log *upstream.UpstreamLog) error {
	_, err := u.db.ExecContext(ctx,
		`INSERT INTO upstream_logs (upstream_id, action, detail, operator, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		log.UpstreamID, log.Action, log.Detail, log.Operator, time.Now().Format(time.RFC3339Nano))
	return err
}

// ListUpstreamLogs 列出上游操作日志
func (u *UpstreamDB) ListUpstreamLogs(ctx context.Context, upstreamID int64, limit int) ([]*upstream.UpstreamLog, error) {
	rows, err := u.db.QueryContext(ctx,
		`SELECT id, upstream_id, action, detail, operator, created_at
		 FROM upstream_logs WHERE upstream_id=? ORDER BY id DESC LIMIT ?`, upstreamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*upstream.UpstreamLog, 0)
	for rows.Next() {
		var l upstream.UpstreamLog
		var created string
		if err := rows.Scan(&l.ID, &l.UpstreamID, &l.Action, &l.Detail, &l.Operator, &created); err != nil {
			return nil, err
		}
		l.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &l)
	}
	return out, rows.Err()
}

// Close 关闭上游数据库
func (u *UpstreamDB) Close() error {
	return u.db.Close()
}

// ---------- 辅助函数 ----------

// scanUpstreamEncrypted 扫描一行到 UpstreamEncrypted
func scanUpstreamEncrypted(s rowScanner) (*upstream.UpstreamEncrypted, error) {
	var e upstream.UpstreamEncrypted
	var createdAt, updatedAt string
	err := s.Scan(&e.ID, &e.Name, &e.Pid, &e.KeyEnc, &e.BaseURL,
		&e.NotifyDomains, &e.Status, &e.Remark, &createdAt, &updatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
	e.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt)
	return &e, nil
}
