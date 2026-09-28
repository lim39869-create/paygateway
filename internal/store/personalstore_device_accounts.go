package store

import "context"

// ---------- 设备 ↔ 账户 多对多绑定（应用双开多账户）----------
//
// 一台物理设备（双开微信/支付宝）可服务多个收款账户：
//   - 主账户存 personal_devices.account_id（兼容旧逻辑 + 金额兜底主校验）
//   - 附加账户存 device_accounts 表（每账户一行）
// 匹配时校验订单账户 ∈ {主账户} ∪ {附加账户集合}。

// AddDeviceAccount 绑定附加账户到设备（幂等：已绑返回 nil）
func (p *PersonalDB) AddDeviceAccount(ctx context.Context, deviceID int64, accountID int64) error {
	_, err := p.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO device_accounts (device_id, account_id, created_at) VALUES (?, ?, ?)`,
		deviceID, accountID, nowStr())
	return err
}

// RemoveDeviceAccount 解绑附加账户（幂等）
func (p *PersonalDB) RemoveDeviceAccount(ctx context.Context, deviceID int64, accountID int64) error {
	_, err := p.db.ExecContext(ctx,
		`DELETE FROM device_accounts WHERE device_id=? AND account_id=?`, deviceID, accountID)
	return err
}

// AccountBound 查询账户是否仍被任何设备绑定
func (p *PersonalDB) AccountBound(ctx context.Context, accountID int64) (bool, error) {
	var n int
	err := p.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM device_accounts WHERE account_id=?`, accountID).Scan(&n)
	return n > 0, err
}

// ListDeviceAccounts 列出设备全部附加账户 ID（不含主账户 account_id）
func (p *PersonalDB) ListDeviceAccounts(ctx context.Context, deviceID int64) ([]int64, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT account_id FROM device_accounts WHERE device_id=? ORDER BY account_id`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeviceIDsOfAccount 反向查询：绑定了该账户（附加绑定或主账户）的所有设备 ID（int64 主键）。
// 一台设备承载多账户多通道（device_accounts 多对多），账户级判定必须走账户→设备反查，
// 不能按 personal_devices.provider 单值过滤（跨通道订单会误判设备离线）。
func (p *PersonalDB) DeviceIDsOfAccount(ctx context.Context, accountID int64) ([]int64, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT device_id FROM device_accounts WHERE account_id=?
		 UNION
		 SELECT id FROM personal_devices WHERE account_id=?
		 ORDER BY 1`, accountID, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	seen := make(map[int64]bool)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// DeviceAccountIDs 返回设备全部绑定账户 ID（主账户 + 附加账户，去重）。
// 供匹配引擎/守卫查询设备可服务的账户集合。参数 d 可传 nil（仅附加账户）。
func DeviceAccountIDs(ctx context.Context, pdb PersonalStore, deviceID int64, primaryAccountID int64) []int64 {
	extra, err := pdb.ListDeviceAccounts(ctx, deviceID)
	if err != nil {
		extra = nil
	}
	seen := make(map[int64]bool)
	var out []int64
	add := func(id int64) {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	add(primaryAccountID)
	for _, id := range extra {
		add(id)
	}
	return out
}
