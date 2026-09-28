package store

import (
	"context"
	"database/sql"

	"paygateway/internal/personal"
)

// ---------- 设备 CRUD ----------

// ListDevices 列出全部设备
func (p *PersonalDB) ListDevices(ctx context.Context) ([]*personal.DeviceEncrypted, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id, device_id, name, account_id, provider, secret_enc, status, last_heartbeat, last_push, ip, adb_host, adb_port, adb_wifi_host, adb_wifi_port, setup_progress, ts_ip, adb_listen, control_state, created_at
		 FROM personal_devices ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*personal.DeviceEncrypted
	for rows.Next() {
		e, err := scanDeviceEncrypted(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// GetDevice 按 device_id 获取设备
func (p *PersonalDB) GetDevice(ctx context.Context, deviceID string) (*personal.DeviceEncrypted, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT id, device_id, name, account_id, provider, secret_enc, status, last_heartbeat, last_push, ip, adb_host, adb_port, adb_wifi_host, adb_wifi_port, setup_progress, ts_ip, adb_listen, control_state, created_at
		 FROM personal_devices WHERE device_id=?`, deviceID)
	return scanDeviceEncrypted(row)
}

// GetDeviceByID 按 ID 获取设备（配置二维码用）
func (p *PersonalDB) GetDeviceByID(ctx context.Context, id int64) (*personal.DeviceEncrypted, error) {
	row := p.db.QueryRowContext(ctx,
		`SELECT id, device_id, name, account_id, provider, secret_enc, status, last_heartbeat, last_push, ip, adb_host, adb_port, adb_wifi_host, adb_wifi_port, setup_progress, ts_ip, adb_listen, control_state, created_at
		 FROM personal_devices WHERE id=?`, id)
	return scanDeviceEncrypted(row)
}

// CreateDevice 创建设备，返回新 ID
func (p *PersonalDB) CreateDevice(ctx context.Context, d *personal.DeviceEncrypted) (int64, error) {
	progress := d.SetupProgress
	if progress == "" {
		progress = "[]"
	}
	res, err := p.db.ExecContext(ctx,
		`INSERT INTO personal_devices (device_id, name, account_id, provider, secret_enc, status, last_heartbeat, last_push, ip, adb_host, adb_port, adb_wifi_host, adb_wifi_port, setup_progress, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.DeviceID, d.Name, d.AccountID, d.Provider, d.SecretEnc,
		d.Status, "", d.LastPush, d.IP, d.AdbHost, d.AdbPort, d.AdbWifiHost, d.AdbWifiPort, progress, nowStr())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateDevice 更新设备
func (p *PersonalDB) UpdateDevice(ctx context.Context, d *personal.DeviceEncrypted) error {
	progress := d.SetupProgress
	if progress == "" {
		progress = "[]"
	}
	_, err := p.db.ExecContext(ctx,
		`UPDATE personal_devices SET name=?, account_id=?, provider=?, secret_enc=?, status=?, last_heartbeat=?, last_push=?, ip=?, adb_host=?, adb_port=?, adb_wifi_host=?, adb_wifi_port=?, setup_progress=?
		 WHERE id=?`,
		d.Name, d.AccountID, d.Provider, d.SecretEnc, d.Status, "", d.LastPush, d.IP, d.AdbHost, d.AdbPort, d.AdbWifiHost, d.AdbWifiPort, progress, d.ID)
	return err
}

// SetDeviceAdb 更新设备的 adb 连接信息（USB serial / host:port / WiFi 备用通道）
func (p *PersonalDB) SetDeviceAdb(ctx context.Context, id int64, host string, port int, wifiHost string, wifiPort int) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE personal_devices SET adb_host=?, adb_port=?, adb_wifi_host=?, adb_wifi_port=? WHERE id=?`, host, port, wifiHost, wifiPort, id)
	return err
}

// DeleteDevice 删除设备
func (p *PersonalDB) DeleteDevice(ctx context.Context, id int64) error {
	_, err := p.db.ExecContext(ctx, `DELETE FROM personal_devices WHERE id=?`, id)
	return err
}

// SetDeviceStatus 设置设备状态
func (p *PersonalDB) SetDeviceStatus(ctx context.Context, id int64, status string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE personal_devices SET status=? WHERE id=?`, status, id)
	return err
}

// SetDevicePushTime 更新最后推送时间
func (p *PersonalDB) SetDevicePushTime(ctx context.Context, deviceID string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE personal_devices SET last_push=? WHERE device_id=?`,
		nowStr(), deviceID)
	return err
}

// SetDeviceSetupProgress 更新引导向导完成度（JSON 数组字符串）
func (p *PersonalDB) SetDeviceSetupProgress(ctx context.Context, id int64, progress string) error {
	if progress == "" {
		progress = "[]"
	}
	_, err := p.db.ExecContext(ctx,
		`UPDATE personal_devices SET setup_progress=? WHERE id=?`, progress, id)
	return err
}

// SetDeviceControlPlane 更新设备的 P2P 控制面状态（心跳上报 ts_ip + adb_listen；reconciler 维护 control_state）
func (p *PersonalDB) SetDeviceControlPlane(ctx context.Context, deviceID, tsIP string, adbListen bool, controlState string) error {
	if controlState == "" {
		controlState = personal.ControlStateTunnelOnly
	}
	_, err := p.db.ExecContext(ctx,
		`UPDATE personal_devices SET ts_ip=?, adb_listen=?, control_state=? WHERE device_id=?`,
		tsIP, boolToInt(adbListen), controlState, deviceID)
	return err
}

// scanDeviceEncrypted 扫描一行到 DeviceEncrypted
func scanDeviceEncrypted(s rowScanner) (*personal.DeviceEncrypted, error) {
	var e personal.DeviceEncrypted
	var adbListen int
	var lastHeartbeat string // 遗留列（listener 弃用后不再维护，仅占位避免改 schema）
	err := s.Scan(&e.ID, &e.DeviceID, &e.Name, &e.AccountID, &e.Provider,
		&e.SecretEnc, &e.Status, &lastHeartbeat, &e.LastPush, &e.IP, &e.AdbHost, &e.AdbPort,
		&e.AdbWifiHost, &e.AdbWifiPort, &e.SetupProgress, &e.TsIP, &adbListen, &e.ControlState, &e.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	e.AdbListen = adbListen
	if e.ControlState == "" {
		e.ControlState = personal.ControlStateTunnelOnly
	}
	return &e, nil
}
