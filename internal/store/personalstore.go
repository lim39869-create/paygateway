// Package store 个人免签通道存储：独立 personal.db
//
// 表：
//   - personal_devices      监听端设备（含加密通信密钥）
//   - personal_pushes       收款事件流水（审计用）
//   - unmatched_payments    无订单收款（人工处理）
//
// 安全设计：独立 SQLite 文件 0600，WAL 模式，设备密钥 AES-256-GCM 加密存储。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"

	_ "modernc.org/sqlite"

	"paygateway/internal/personal"
)

// PersonalStore 个人免签通道存储接口（仅 admin + personal 通道使用）
type PersonalStore interface {
	// 设备
	ListDevices(ctx context.Context) ([]*personal.DeviceEncrypted, error)
	GetDevice(ctx context.Context, deviceID string) (*personal.DeviceEncrypted, error)
	GetDeviceByID(ctx context.Context, id int64) (*personal.DeviceEncrypted, error)
	CreateDevice(ctx context.Context, d *personal.DeviceEncrypted) (int64, error)
	UpdateDevice(ctx context.Context, d *personal.DeviceEncrypted) error
	DeleteDevice(ctx context.Context, id int64) error
	SetDeviceStatus(ctx context.Context, id int64, status string) error
	SetDevicePushTime(ctx context.Context, deviceID string) error
	SetDeviceAdb(ctx context.Context, id int64, host string, port int, wifiHost string, wifiPort int) error
	SetDeviceSetupProgress(ctx context.Context, id int64, progress string) error
	SetDeviceControlPlane(ctx context.Context, deviceID, tsIP string, adbListen bool, controlState string) error

	// 设备 ↔ 账户 多对多绑定（应用双开多账户：一台设备服务多个收款账户）
	// 设备主账户仍存于 personal_devices.account_id（兼容旧逻辑），附加账户存 device_accounts 表
	AddDeviceAccount(ctx context.Context, deviceID int64, accountID int64) error
	RemoveDeviceAccount(ctx context.Context, deviceID int64, accountID int64) error
	AccountBound(ctx context.Context, accountID int64) (bool, error)
	ListDeviceAccounts(ctx context.Context, deviceID int64) ([]int64, error)

	// 收款推送流水
	InsertPush(ctx context.Context, p *personal.Push) error
	ListPushes(ctx context.Context, limit int) ([]*personal.Push, error)
	CountPushes(ctx context.Context) (int64, error)
	PushExists(ctx context.Context, deviceID, rawHash string) (bool, error)

	// 无订单收款
	CreateUnmatched(ctx context.Context, u *personal.UnmatchedPayment) (int64, error)
	ListUnmatched(ctx context.Context, status string, limit int) ([]*personal.UnmatchedPayment, error)
	CountUnmatched(ctx context.Context, status string) (int64, error)
	ResolveUnmatched(ctx context.Context, id int64, status, handler string) error

	// 动态收款码
	EnsureQrcodeSchema(ctx context.Context) error
	UpsertQrcode(ctx context.Context, q *personal.DeviceQrcode) (int64, error)
	GetLatestQrcode(ctx context.Context, deviceID string) (*personal.DeviceQrcode, error)
	GetLatestQrcodeByPlatform(ctx context.Context, deviceID, platform string) (*personal.DeviceQrcode, error)
	GetLatestQrcodeByPlatformUser(ctx context.Context, deviceID, platform string, userID int) (*personal.DeviceQrcode, error)
	ListQrcodes(ctx context.Context, limit int) ([]*personal.DeviceQrcode, error)
	DeleteExpiredQrcodes(ctx context.Context) (int64, error)
	DeleteQrcodeByDevice(ctx context.Context, deviceID string) error
	DeleteQrcode(ctx context.Context, deviceID, platform string, userID int) error

	// 监听会话（事件驱动收款监听，docs/PLAN-EVENT-MONITOR.md）

	Close() error
}

// PersonalDB 独立个人通道数据库
type PersonalDB struct {
	db *sql.DB
}

// NewPersonalDB 打开/创建独立个人通道数据库（0600 + WAL）
func NewPersonalDB(path string) (*PersonalDB, error) {
	fd, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建个人通道数据库文件失败: %w", err)
	}
	fd.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return nil, fmt.Errorf("设置个人通道数据库权限失败: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=10000&_synchronous=FULL")
	if err != nil {
		return nil, fmt.Errorf("打开个人通道数据库失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	p := &PersonalDB{db: db}
	if err := p.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return p, nil
}

// migrate 建表（幂等）
func (p *PersonalDB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS personal_devices (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id       TEXT NOT NULL UNIQUE,
			name            TEXT NOT NULL DEFAULT '',
			account_id      INTEGER NOT NULL DEFAULT 0,
			provider        TEXT NOT NULL DEFAULT '',
			secret_enc      TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'offline',
			last_heartbeat  TEXT NOT NULL DEFAULT '',
			last_push       TEXT NOT NULL DEFAULT '',
			ip              TEXT NOT NULL DEFAULT '',
			adb_host        TEXT NOT NULL DEFAULT '',
			adb_port        INTEGER NOT NULL DEFAULT 0,
			adb_wifi_host   TEXT NOT NULL DEFAULT '',
			adb_wifi_port   INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_personal_devices_status ON personal_devices(status)`,
		`CREATE INDEX IF NOT EXISTS idx_personal_devices_account ON personal_devices(account_id)`,
		`CREATE TABLE IF NOT EXISTS personal_pushes (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id       TEXT NOT NULL,
			provider        TEXT NOT NULL,
			amount_minor    INTEGER NOT NULL DEFAULT 0,
			note            TEXT NOT NULL DEFAULT '',
			raw_hash        TEXT NOT NULL DEFAULT '',
			matched_order   TEXT NOT NULL DEFAULT '',
			match_result    TEXT NOT NULL DEFAULT '',
			risk_text       TEXT NOT NULL DEFAULT '',
			raw             TEXT NOT NULL DEFAULT '',
			created_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_personal_pushes_device ON personal_pushes(device_id, created_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_personal_pushes_dedup ON personal_pushes(device_id, raw_hash) WHERE raw_hash != ''`,
		`CREATE TABLE IF NOT EXISTS unmatched_payments (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			provider        TEXT NOT NULL,
			amount_minor    INTEGER NOT NULL DEFAULT 0,
			note            TEXT NOT NULL DEFAULT '',
			raw             TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'pending',
			handler         TEXT NOT NULL DEFAULT '',
			payer           TEXT NOT NULL DEFAULT '',
			device_id       TEXT NOT NULL DEFAULT '',
			risk_tags       TEXT NOT NULL DEFAULT '',
			risk_level      INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_unmatched_status ON unmatched_payments(status, created_at)`,
		`CREATE TABLE IF NOT EXISTS device_accounts (
			device_id   INTEGER NOT NULL,
			account_id  INTEGER NOT NULL,
			created_at  TEXT NOT NULL,
			PRIMARY KEY (device_id, account_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_device_accounts_account ON device_accounts(account_id)`,
		`CREATE TABLE IF NOT EXISTS device_qrcodes (
			id              INTEGER PRIMARY KEY AUTOINCREMENT,
			device_id       TEXT NOT NULL,
			platform        TEXT NOT NULL DEFAULT '',
			qr_content      TEXT NOT NULL DEFAULT '',
			account         TEXT NOT NULL DEFAULT '',
			expire_in       INTEGER NOT NULL DEFAULT 0,
			expire_at       TEXT NOT NULL DEFAULT '',
			status          TEXT NOT NULL DEFAULT 'active',
			pending_fetch   INTEGER NOT NULL DEFAULT 0,
			created_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_device_qrcodes_device ON device_qrcodes(device_id)`,
		`CREATE TABLE IF NOT EXISTS monitor_sessions (
			account_id  INTEGER PRIMARY KEY,
			device_id   TEXT NOT NULL DEFAULT '',
			provider    TEXT NOT NULL DEFAULT '',
			state       TEXT NOT NULL DEFAULT 'idle',
			active      INTEGER NOT NULL DEFAULT 0,
			retry       INTEGER NOT NULL DEFAULT 0,
			last_error  TEXT NOT NULL DEFAULT '',
			updated_at  TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS ledger_checksums (
			device_id   TEXT NOT NULL,
			account_id  INTEGER NOT NULL DEFAULT 0,
			fingerprint TEXT NOT NULL,
			created_at  TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (device_id, account_id, fingerprint)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ledger_checksums_dev ON ledger_checksums(device_id, account_id)`,
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
	}
	for _, stmt := range stmts {
		if _, err := p.db.Exec(stmt); err != nil {
			return fmt.Errorf("个人通道数据库迁移失败: %w", err)
		}
	}
	// 兼容迁移：老库补 risk_text 列（已存在则忽略错误）
	_, _ = p.db.Exec(`ALTER TABLE personal_pushes ADD COLUMN risk_text TEXT NOT NULL DEFAULT ''`)
	// 兼容迁移：unmatched_payments 补风控留痕列（payer/device/risk_tags/risk_level）
	_, _ = p.db.Exec(`ALTER TABLE unmatched_payments ADD COLUMN payer TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE unmatched_payments ADD COLUMN device_id TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE unmatched_payments ADD COLUMN risk_tags TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE unmatched_payments ADD COLUMN risk_level INTEGER NOT NULL DEFAULT 0`)
	// 索引在 ALTER 之后建（老库 payer 列由上面 ALTER 补齐；放 stmts 会让老库启动直接失败）——生产事故 2026-09-14
	_, _ = p.db.Exec(`CREATE INDEX IF NOT EXISTS idx_unmatched_payer ON unmatched_payments(payer, created_at)`)
	// 兼容迁移：老库补 space 列（通知来源空间：0=主空间，999=双开(XSpace)）
	_, _ = p.db.Exec(`ALTER TABLE personal_pushes ADD COLUMN space INTEGER NOT NULL DEFAULT 0`)
	// 兼容迁移：老库补 adb 连接列（web 可视化操作设备用）
	_, _ = p.db.Exec(`ALTER TABLE personal_devices ADD COLUMN adb_host TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE personal_devices ADD COLUMN adb_port INTEGER NOT NULL DEFAULT 0`)
	// 兼容迁移：老库补 WiFi 备用通道列（USB 断线自动切换用）
	_, _ = p.db.Exec(`ALTER TABLE personal_devices ADD COLUMN adb_wifi_host TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE personal_devices ADD COLUMN adb_wifi_port INTEGER NOT NULL DEFAULT 0`)
	// 兼容迁移：老库补 setup_progress 列（引导向导完成度，JSON 数组字符串；缺省 '[]' = 未配置）
	_, _ = p.db.Exec(`ALTER TABLE personal_devices ADD COLUMN setup_progress TEXT NOT NULL DEFAULT '[]'`)
	// 兼容迁移：老库补 device_qrcodes.account 列（收款账户标识，昵称/微信号）
	_, _ = p.db.Exec(`ALTER TABLE device_qrcodes ADD COLUMN account TEXT NOT NULL DEFAULT ''`)
	// 兼容迁移：P2P 组网控制面（docs/P2P-ADB-RECOVERY.md Phase 1）
	//   ts_ip          : 设备组网 IP（Tailscale 100.x.x.x，未上线为空）
	//   adb_listen     : adbd TCP 监听是否就绪（心跳上报，0/1）
	//   control_state  : 控制面状态：usb / tunnel_only
	_, _ = p.db.Exec(`ALTER TABLE personal_devices ADD COLUMN ts_ip TEXT NOT NULL DEFAULT ''`)
	_, _ = p.db.Exec(`ALTER TABLE personal_devices ADD COLUMN adb_listen INTEGER NOT NULL DEFAULT 0`)
	_, _ = p.db.Exec(`ALTER TABLE personal_devices ADD COLUMN control_state TEXT NOT NULL DEFAULT 'tunnel_only'`)
	return nil
}

// Close 关闭数据库
func (p *PersonalDB) Close() error { return p.db.Close() }
