package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"paygateway/internal/personal"
)

// ---------- 设备动态收款码 CRUD ----------

// EnsureQrcodeSchema 迁移 device_qrcodes 表，确保含 user_id 列。
// 首次部署或旧表升级时自动执行（幂等：列已存在则跳过）。
func (p *PersonalDB) EnsureQrcodeSchema(ctx context.Context) error {
	// 1. 检查 user_id 列是否已存在
	var count int
	err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('device_qrcodes') WHERE name='user_id'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("检查 device_qrcodes 结构: %w", err)
	}
	if count > 0 {
		return nil // 已迁移
	}
	// 2. 迁移：旧表重命名 → 建新表 → 复制数据 → 删旧表
	// 旧唯一约束 (device_id, platform) → 新唯一约束 (device_id, platform, user_id)
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始迁移事务: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `ALTER TABLE device_qrcodes RENAME TO device_qrcodes_old`)
	if err != nil {
		return fmt.Errorf("重命名旧表: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		CREATE TABLE device_qrcodes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id TEXT NOT NULL,
			platform TEXT NOT NULL DEFAULT '',
			user_id INTEGER NOT NULL DEFAULT 0,
			qr_content TEXT NOT NULL DEFAULT '',
			account TEXT NOT NULL DEFAULT '',
			expire_in INTEGER NOT NULL DEFAULT 0,
			expire_at TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			pending_fetch INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			UNIQUE(device_id, platform, user_id)
		)`)
	if err != nil {
		return fmt.Errorf("建新表: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO device_qrcodes (id, device_id, platform, user_id, qr_content, account, expire_in, expire_at, status, pending_fetch, created_at)
		SELECT id, device_id, platform, 0, qr_content, account, expire_in, expire_at, status, pending_fetch, created_at
		FROM device_qrcodes_old`)
	if err != nil {
		return fmt.Errorf("迁移数据: %w", err)
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE device_qrcodes_old`)
	if err != nil {
		return fmt.Errorf("删除旧表: %w", err)
	}
	return tx.Commit()
}

// UpsertQrcode 写入/更新设备指定平台+用户空间的收款码（每设备每平台每用户空间幂等覆盖）。
// 同一设备可同时持有 wechat(主空间) + wechat(双开) + alipay(主空间) + alipay(双开) 四条互不覆盖。
func (p *PersonalDB) UpsertQrcode(ctx context.Context, q *personal.DeviceQrcode) (int64, error) {
	var id int64
	err := p.db.QueryRowContext(ctx,
		`SELECT id FROM device_qrcodes WHERE device_id=? AND platform=? AND user_id=? ORDER BY id DESC LIMIT 1`,
		q.DeviceID, q.Platform, q.UserID).Scan(&id)
	if err == nil {
		_, uerr := p.db.ExecContext(ctx,
			`UPDATE device_qrcodes SET qr_content=?, account=?, expire_in=?, expire_at=?, status=?, created_at=? WHERE id=?`,
			q.QRContent, q.Account, q.ExpireIn, q.ExpireAt, q.Status, q.CreatedAt, id)
		return id, uerr
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	res, ierr := p.db.ExecContext(ctx,
		`INSERT INTO device_qrcodes (device_id, platform, user_id, qr_content, account, expire_in, expire_at, status, pending_fetch, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		q.DeviceID, q.Platform, q.UserID, q.QRContent, q.Account, q.ExpireIn, q.ExpireAt, q.Status, q.CreatedAt)
	if ierr != nil {
		return 0, ierr
	}
	return res.LastInsertId()
}

// GetLatestQrcode 获取设备最新收款码（任意平台、任意用户空间）
func (p *PersonalDB) GetLatestQrcode(ctx context.Context, deviceID string) (*personal.DeviceQrcode, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT id, device_id, platform, user_id, qr_content, account, expire_in, expire_at, status, pending_fetch, created_at
		 FROM device_qrcodes WHERE device_id=? ORDER BY id DESC LIMIT 1`, deviceID)
	return scanQrcode(row)
}

// GetLatestQrcodeByPlatform 获取设备指定平台的最新收款码（跨用户空间，取最新一条；用于旧版去重/兼容）
func (p *PersonalDB) GetLatestQrcodeByPlatform(ctx context.Context, deviceID, platform string) (*personal.DeviceQrcode, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT id, device_id, platform, user_id, qr_content, account, expire_in, expire_at, status, pending_fetch, created_at
		 FROM device_qrcodes WHERE device_id=? AND platform=? ORDER BY id DESC LIMIT 1`, deviceID, platform)
	return scanQrcode(row)
}

// GetLatestQrcodeByPlatformUser 获取设备指定平台+用户空间的最新收款码（续期调度用）
func (p *PersonalDB) GetLatestQrcodeByPlatformUser(ctx context.Context, deviceID, platform string, userID int) (*personal.DeviceQrcode, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT id, device_id, platform, user_id, qr_content, account, expire_in, expire_at, status, pending_fetch, created_at
		 FROM device_qrcodes WHERE device_id=? AND platform=? AND user_id=? ORDER BY id DESC LIMIT 1`,
		deviceID, platform, userID)
	return scanQrcode(row)
}

// ListQrcodes 列出最新收款码（按 id 倒序）
func (p *PersonalDB) ListQrcodes(ctx context.Context, limit int) ([]*personal.DeviceQrcode, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, device_id, platform, user_id, qr_content, account, expire_in, expire_at, status, pending_fetch, created_at
		 FROM device_qrcodes ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*personal.DeviceQrcode
	for rows.Next() {
		q, err := scanQrcode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

func scanQrcode(row interface{ Scan(dest ...any) error }) (*personal.DeviceQrcode, error) {
	q := &personal.DeviceQrcode{}
	err := row.Scan(&q.ID, &q.DeviceID, &q.Platform, &q.UserID, &q.QRContent, &q.Account, &q.ExpireIn, &q.ExpireAt, &q.Status, &q.PendingFetch, &q.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return q, nil
}

// DeleteExpiredQrcodes 清理过期收款码：删除已过期或 status=expired 的记录，
// 但每设备每平台每用户空间保留最新一条（防清空后无码可用）。
// 返回删除的行数。
func (p *PersonalDB) DeleteExpiredQrcodes(ctx context.Context) (int64, error) {
	// 每设备每平台每用户空间最新一条的 id（无论状态都保留）
	keep := `SELECT MAX(id) FROM device_qrcodes GROUP BY device_id, platform, user_id`
	res, err := p.db.ExecContext(ctx, `
		DELETE FROM device_qrcodes
		WHERE id NOT IN (`+keep+`)
		  AND (status = 'expired' OR expire_at <= ?)`,
		time.Now().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteQrcodeByDevice 删除指定设备的所有收款码记录（设备解绑/删除时用）
func (p *PersonalDB) DeleteQrcodeByDevice(ctx context.Context, deviceID string) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM device_qrcodes WHERE device_id=?`, deviceID)
	return err
}

// DeleteQrcode 删除单条收款码记录（deviceID+platform+userID 唯一定位）
func (p *PersonalDB) DeleteQrcode(ctx context.Context, deviceID, platform string, userID int) error {
	_, err := p.db.ExecContext(ctx,
		`DELETE FROM device_qrcodes WHERE device_id=? AND platform=? AND user_id=?`,
		deviceID, platform, userID)
	return err
}

// ExpireAtStr 计算过期时间字符串（RFC3339，now + expireIn 秒；expireIn<=0 默认 90s）
func ExpireAtStr(expireIn int64) string {
	if expireIn <= 0 {
		expireIn = 90
	}
	return time.Now().Add(time.Duration(expireIn) * time.Second).Format(time.RFC3339)
}
