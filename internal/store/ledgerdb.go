package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// LedgerStore 账本存储接口（只增不改，独立数据库）
type LedgerStore interface {
	AppendLedger(ctx context.Context, prevHash, payloadHash, entryHash, signature string, eventID int64) error
	GetLastLedgerEntry(ctx context.Context) (*LedgerEntry, error)
	CountLedger(ctx context.Context) (int64, error)
	GetLedgerEntry(ctx context.Context, id int64) (*LedgerEntry, error)
	ListLedger(ctx context.Context, limit int) ([]*LedgerEntry, error)
	// HasLedgerEvent 按 eventID（订单ID）查询账本是否已有记录（崩溃恢复去重用）
	HasLedgerEvent(ctx context.Context, eventID int64) (bool, error)
	CreateSnapshot(ctx context.Context, date string, lastHash, signature string) error
	GetLatestSnapshot(ctx context.Context) (*Snapshot, error)
	ListSnapshots(ctx context.Context, limit int) ([]*Snapshot, error)
	Close() error
}

// LedgerDB 独立账本数据库：只存 audit_ledger + daily_snapshots
// 文件权限 0600，WAL 模式，触发器防 UPDATE/DELETE
type LedgerDB struct {
	db *sql.DB
}

// NewLedgerDB 打开/创建独立账本数据库
func NewLedgerDB(path string) (*LedgerDB, error) {
	// 先创建空文件以确保我们可以设置权限
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建账本数据库文件失败: %w", err)
	}
	fd.Close()

	// 强制设置权限为 0600（只有文件所有者可读写）
	if err := os.Chmod(path, 0600); err != nil {
		return nil, fmt.Errorf("设置账本数据库权限失败: %w", err)
	}

	// 打开连接：WAL 模式防崩溃 + 严格外键
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=FULL")
	if err != nil {
		return nil, fmt.Errorf("打开账本数据库失败: %w", err)
	}

	// 连接池：账本写入频率低，单连接更安全
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	l := &LedgerDB{db: db}
	if err := l.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

// migrate 创建账本表 + 触发器（只增不改）
func (l *LedgerDB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS audit_ledger (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			prev_hash   TEXT NOT NULL,
			payload_hash TEXT NOT NULL,
			entry_hash  TEXT NOT NULL,
			signature   TEXT NOT NULL,
			event_id    INTEGER,
			created_at  TEXT NOT NULL
		)`,
		// 禁止 UPDATE — 任何修改账本条目的尝试都被拒绝
		`CREATE TRIGGER IF NOT EXISTS trg_ledger_no_update BEFORE UPDATE ON audit_ledger
		 BEGIN SELECT RAISE(ABORT, 'audit_ledger is append-only: UPDATE denied'); END`,
		// 禁止 DELETE — 任何删除账本条目的尝试都被拒绝
		`CREATE TRIGGER IF NOT EXISTS trg_ledger_no_delete BEFORE DELETE ON audit_ledger
		 BEGIN SELECT RAISE(ABORT, 'audit_ledger is append-only: DELETE denied'); END`,
		`CREATE TABLE IF NOT EXISTS daily_snapshots (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			snapshot_date TEXT NOT NULL UNIQUE,
			last_hash     TEXT NOT NULL,
			signature     TEXT NOT NULL,
			created_at    TEXT NOT NULL
		)`,
		// 快照同样防篡改
		`CREATE TRIGGER IF NOT EXISTS trg_snapshot_no_update BEFORE UPDATE ON daily_snapshots
		 BEGIN SELECT RAISE(ABORT, 'daily_snapshots is append-only: UPDATE denied'); END`,
		`CREATE TRIGGER IF NOT EXISTS trg_snapshot_no_delete BEFORE DELETE ON daily_snapshots
		 BEGIN SELECT RAISE(ABORT, 'daily_snapshots is append-only: DELETE denied'); END`,
		// 启用 WAL 模式（持久化）
		`PRAGMA journal_mode=WAL`,
		// 同步模式 FULL — 每次写入都等待数据落盘
		`PRAGMA synchronous=FULL`,
	}
	for _, stmt := range stmts {
		if _, err := l.db.Exec(stmt); err != nil {
			return fmt.Errorf("账本数据库迁移失败: %w", err)
		}
	}
	return nil
}

// ---------- 账本操作（只增不改） ----------

func (l *LedgerDB) AppendLedger(ctx context.Context, prevHash, payloadHash, entryHash, signature string, eventID int64) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO audit_ledger (prev_hash, payload_hash, entry_hash, signature, event_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		prevHash, payloadHash, entryHash, signature, eventID, nowStr())
	return err
}

func (l *LedgerDB) GetLastLedgerEntry(ctx context.Context) (*LedgerEntry, error) {
	row := l.db.QueryRowContext(ctx,
		`SELECT id, prev_hash, payload_hash, entry_hash, signature, event_id, created_at
		 FROM audit_ledger ORDER BY id DESC LIMIT 1`)
	var e LedgerEntry
	var created string
	if err := row.Scan(&e.ID, &e.PrevHash, &e.PayloadHash, &e.EntryHash, &e.Signature, &e.EventID, &created); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return &e, nil
}

func (l *LedgerDB) CountLedger(ctx context.Context) (int64, error) {
	var n int64
	if err := l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_ledger`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (l *LedgerDB) GetLedgerEntry(ctx context.Context, id int64) (*LedgerEntry, error) {
	row := l.db.QueryRowContext(ctx,
		`SELECT id, prev_hash, payload_hash, entry_hash, signature, event_id, created_at
		 FROM audit_ledger WHERE id=?`, id)
	var e LedgerEntry
	var created string
	if err := row.Scan(&e.ID, &e.PrevHash, &e.PayloadHash, &e.EntryHash, &e.Signature, &e.EventID, &created); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return &e, nil
}

// HasLedgerEvent 按 eventID（订单ID）查询账本是否已有记录（崩溃恢复去重用）
func (l *LedgerDB) HasLedgerEvent(ctx context.Context, eventID int64) (bool, error) {
	var n int64
	if err := l.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_ledger WHERE event_id=?`, eventID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (l *LedgerDB) ListLedger(ctx context.Context, limit int) ([]*LedgerEntry, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, prev_hash, payload_hash, entry_hash, signature, event_id, created_at
		 FROM audit_ledger ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*LedgerEntry{}
	for rows.Next() {
		var e LedgerEntry
		var created string
		if err := rows.Scan(&e.ID, &e.PrevHash, &e.PayloadHash, &e.EntryHash, &e.Signature, &e.EventID, &created); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ---------- 快照操作 ----------

func (l *LedgerDB) ListSnapshots(ctx context.Context, limit int) ([]*Snapshot, error) {
	rows, err := l.db.QueryContext(ctx,
		`SELECT id, snapshot_date, last_hash, signature, created_at FROM daily_snapshots ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Snapshot{}
	for rows.Next() {
		var sn Snapshot
		var created string
		if err := rows.Scan(&sn.ID, &sn.SnapshotDate, &sn.LastHash, &sn.Signature, &created); err != nil {
			return nil, err
		}
		sn.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &sn)
	}
	return out, rows.Err()
}

func (l *LedgerDB) CreateSnapshot(ctx context.Context, date string, lastHash, signature string) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO daily_snapshots (snapshot_date, last_hash, signature, created_at) VALUES (?, ?, ?, ?)`,
		date, lastHash, signature, nowStr())
	return err
}

func (l *LedgerDB) GetLatestSnapshot(ctx context.Context) (*Snapshot, error) {
	row := l.db.QueryRowContext(ctx,
		`SELECT id, snapshot_date, last_hash, signature, created_at FROM daily_snapshots ORDER BY id DESC LIMIT 1`)
	var sn Snapshot
	var created string
	if err := row.Scan(&sn.ID, &sn.SnapshotDate, &sn.LastHash, &sn.Signature, &created); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	sn.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return &sn, nil
}

// Close 关闭账本数据库
func (l *LedgerDB) Close() error {
	return l.db.Close()
}
