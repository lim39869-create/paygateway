// 老库迁移回归测试：旧 schema（无 payer 列）→ NewPersonalDB 必须成功补列+建索引。
// 生产事故 2026-09-14：idx_unmatched_payer 放主迁移列表，老库启动直接崩。
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"paygateway/internal/personal"
)

func TestPersonalDBMigratesOldUnmatchedSchema(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "personal.db")

	// 先手工建旧 schema（无 payer/device_id/risk_tags/risk_level 列）
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("打开旧库失败: %v", err)
	}
	if _, err := raw.Exec(`CREATE TABLE unmatched_payments (
		id INTEGER PRIMARY KEY AUTOINCREMENT, provider TEXT NOT NULL,
		amount_minor INTEGER NOT NULL DEFAULT 0, note TEXT NOT NULL DEFAULT '',
		raw TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'pending',
		handler TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("建旧表失败: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO unmatched_payments (provider, amount_minor, created_at) VALUES ('alipay', 10, '2026-09-14T10:00:00+08:00')`); err != nil {
		t.Fatalf("插旧数据失败: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	pdb, err := NewPersonalDB(dbPath)
	if err != nil {
		t.Fatalf("旧库迁移应成功: %v", err)
	}
	defer pdb.Close()

	list, err := pdb.ListUnmatched(context.Background(), "", 10)
	if err != nil {
		t.Fatalf("ListUnmatched 应读到旧数据: %v", err)
	}
	if len(list) != 1 || list[0].AmountMinor != 10 {
		t.Fatalf("旧行应保留: %+v", list)
	}
	if _, err := pdb.CreateUnmatched(context.Background(), &personal.UnmatchedPayment{Provider: "wechat", AmountMinor: 5}); err != nil {
		t.Fatalf("新库写入（含风控列）应成功: %v", err)
	}
}
