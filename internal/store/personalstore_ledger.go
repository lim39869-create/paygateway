package store

import (
	"context"
	"strings"
)

// ledgerSeenTable ledger_checksums：账本页增量检测基线持久化（docs/PLAN-ADB-LEDGER-IMPL.md §6.2）。
// 每 (device_id, account_id) 保存已见指纹集合（时间|金额），服务端重启后恢复，
// 保证重启期间的新收款不因基线重建而漏检。集合小（可见行 ≤ 数十），覆盖写即可。
const ledgerSeenTable = `CREATE TABLE IF NOT EXISTS ledger_checksums (
	device_id   TEXT NOT NULL,
	account_id  INTEGER NOT NULL DEFAULT 0,
	fingerprint TEXT NOT NULL,
	created_at  TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (device_id, account_id, fingerprint)
)`

// LoadSeen 加载某 (设备, 账户) 已见指纹集合。无记录返回空切片（nil, 非错误）。
// 实现 ledgermon.SeenStore 接口（ledgermon 通过最小接口注入，避免与 PersonalStore 全接口耦合）。
func (p *PersonalDB) LoadSeen(ctx context.Context, deviceID string, accountID int64) ([]string, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT fingerprint FROM ledger_checksums WHERE device_id = ? AND account_id = ?`,
		deviceID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			return nil, err
		}
		out = append(out, fp)
	}
	return out, rows.Err()
}

// SaveSeen 覆盖写某 (设备, 账户) 的已见指纹集合（先删后插，单连接串行安全）。
func (p *PersonalDB) SaveSeen(ctx context.Context, deviceID string, accountID int64, fingerprints []string) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM ledger_checksums WHERE device_id = ? AND account_id = ?`, deviceID, accountID); err != nil {
		return err
	}
	now := nowStr()
	for _, fp := range fingerprints {
		if strings.TrimSpace(fp) == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO ledger_checksums (device_id, account_id, fingerprint, created_at) VALUES (?,?,?,?)`,
			deviceID, accountID, fp, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearSeen 清理某 (设备, 账户) 基线（设备解绑/账户变更时用）
func (p *PersonalDB) ClearSeen(ctx context.Context, deviceID string, accountID int64) error {
	_, err := p.db.ExecContext(ctx,
		`DELETE FROM ledger_checksums WHERE device_id = ? AND account_id = ?`, deviceID, accountID)
	return err
}
