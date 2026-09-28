package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// LogDB 独立日志数据库：只存 app_logs，与订单主库分离，支持长期保存。
// 文件权限 0600，WAL 模式。日志只增不删（由外部策略清理），
// 避免日志膨胀拖累订单主库的读写性能，且便于独立备份/迁移/清理。
type LogDB struct {
	db *sql.DB
}

// NewLogDB 打开/创建独立日志数据库
func NewLogDB(path string) (*LogDB, error) {
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建日志数据库文件失败: %w", err)
	}
	fd.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return nil, fmt.Errorf("设置日志数据库权限失败: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL")
	if err != nil {
		return nil, fmt.Errorf("打开日志数据库失败: %w", err)
	}
	// 日志写入并发低，单连接足够且避免锁竞争
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	l := &LogDB{db: db}
	if err := l.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return l, nil
}

// migrate 创建日志表 + 索引（与主库 app_logs 同构）
func (l *LogDB) migrate() error {
	stmts := []string{
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
		`PRAGMA journal_mode=WAL`,
	}
	for _, stmt := range stmts {
		if _, err := l.db.Exec(stmt); err != nil {
			return fmt.Errorf("日志数据库迁移失败: %w", err)
		}
	}
	return nil
}

// InsertLog 写入一条日志（实现 logger.DBWriter）
func (l *LogDB) InsertLog(ctx context.Context, level, scope, message, traceID, fields string) error {
	_, err := l.db.ExecContext(ctx,
		`INSERT INTO app_logs (level, scope, message, trace_id, fields, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		level, scope, message, traceID, fields, nowStr())
	return err
}

// QueryLogs 分页查询日志
func (l *LogDB) QueryLogs(ctx context.Context, level, scope string, limit, offset int) ([]*LogRecord, error) {
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
	rows, err := l.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*LogRecord{}
	for rows.Next() {
		var r LogRecord
		var created string
		if err := rows.Scan(&r.ID, &r.Level, &r.Scope, &r.Message, &r.TraceID, &r.Fields, &created); err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		out = append(out, &r)
	}
	return out, rows.Err()
}

// CountLogs 按条件统计日志数
func (l *LogDB) CountLogs(ctx context.Context, level, scope string) (int64, error) {
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
	if err := l.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// Close 关闭日志数据库
func (l *LogDB) Close() error {
	return l.db.Close()
}
