package ledger

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"paygateway/internal/store"
)

// mockLedgerStore 内存版 LedgerStore，用于单元测试
type mockLedgerStore struct {
	mu        sync.Mutex
	entries   []*store.LedgerEntry
	snapshots []*store.Snapshot
	nextID    int64
}

func (m *mockLedgerStore) AppendLedger(_ context.Context, prevHash, payloadHash, entryHash, signature string, eventID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	m.entries = append(m.entries, &store.LedgerEntry{
		ID:          m.nextID,
		PrevHash:    prevHash,
		PayloadHash: payloadHash,
		EntryHash:   entryHash,
		Signature:   signature,
		EventID:     eventID,
		CreatedAt:   time.Now(),
	})
	return nil
}

func (m *mockLedgerStore) GetLastLedgerEntry(_ context.Context) (*store.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) == 0 {
		return nil, nil
	}
	return m.entries[len(m.entries)-1], nil
}

func (m *mockLedgerStore) CountLedger(_ context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return int64(len(m.entries)), nil
}

func (m *mockLedgerStore) GetLedgerEntry(_ context.Context, id int64) (*store.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.ID == id {
			return e, nil
		}
	}
	return nil, nil
}

func (m *mockLedgerStore) HasLedgerEvent(_ context.Context, eventID int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.entries {
		if e.EventID == eventID {
			return true, nil
		}
	}
	return false, nil
}

func (m *mockLedgerStore) ListLedger(_ context.Context, limit int) ([]*store.LedgerEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.entries) {
		limit = len(m.entries)
	}
	out := make([]*store.LedgerEntry, 0, limit)
	for i := len(m.entries) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.entries[i])
	}
	return out, nil
}

func (m *mockLedgerStore) CreateSnapshot(_ context.Context, date, lastHash, signature string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots = append(m.snapshots, &store.Snapshot{
		ID:           int64(len(m.snapshots) + 1),
		SnapshotDate: date,
		LastHash:     lastHash,
		Signature:    signature,
		CreatedAt:    time.Now(),
	})
	return nil
}

func (m *mockLedgerStore) GetLatestSnapshot(_ context.Context) (*store.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.snapshots) == 0 {
		return nil, nil
	}
	return m.snapshots[len(m.snapshots)-1], nil
}

func (m *mockLedgerStore) ListSnapshots(_ context.Context, limit int) ([]*store.Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.snapshots) {
		limit = len(m.snapshots)
	}
	out := make([]*store.Snapshot, 0, limit)
	for i := len(m.snapshots) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.snapshots[i])
	}
	return out, nil
}

func (m *mockLedgerStore) Close() error { return nil }

// newTestLedger 创建带新密钥的测试账本
func newTestLedger(t *testing.T) (*Ledger, *mockLedgerStore) {
	t.Helper()
	m := &mockLedgerStore{}
	// 传空私钥 → 自动生成新密钥对
	lg, err := New(m, "")
	if err != nil {
		t.Fatalf("创建账本失败: %v", err)
	}
	return lg, m
}

// TestAppendAndVerify 测试：追加记录 + 全链校验通过
func TestAppendAndVerify(t *testing.T) {
	lg, m := newTestLedger(t)
	ctx := context.Background()

	// 追加 3 条记录
	for i := int64(1); i <= 3; i++ {
		payload := "order=TEST001&amount=100&status=PAID&event=" + itoa(i)
		if err := lg.Append(ctx, i, payload); err != nil {
			t.Fatalf("追加第 %d 条失败: %v", i, err)
		}
	}

	// 数量正确
	count, _ := m.CountLedger(ctx)
	if count != 3 {
		t.Fatalf("账本条目数 = %d, 期望 3", count)
	}

	// 全链校验通过
	res, err := lg.Verify(ctx)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if !res.Valid {
		t.Fatalf("账本应有效，实际错误: %v", res.Errors)
	}
	if res.TotalEntries != 3 {
		t.Fatalf("校验条目数 = %d, 期望 3", res.TotalEntries)
	}
}

// TestHashChainLinked 测试：哈希链正确连接（每条记录包含上一条哈希）
func TestHashChainLinked(t *testing.T) {
	lg, m := newTestLedger(t)
	ctx := context.Background()

	_ = lg.Append(ctx, 1, "payload-1")
	_ = lg.Append(ctx, 2, "payload-2")
	_ = lg.Append(ctx, 3, "payload-3")

	m.mu.Lock()
	defer m.mu.Unlock()

	// 检查哈希链
	if m.entries[0].PrevHash != "" {
		t.Errorf("第一条 prev_hash 应为空(创世)，实际 %q", m.entries[0].PrevHash)
	}
	if m.entries[1].PrevHash != m.entries[0].EntryHash {
		t.Errorf("第2条 prev_hash 应等于第1条 entry_hash")
	}
	if m.entries[2].PrevHash != m.entries[1].EntryHash {
		t.Errorf("第3条 prev_hash 应等于第2条 entry_hash")
	}
}

// TestTamperDetected 测试：篡改任意条目 → 校验失败
func TestTamperDetected(t *testing.T) {
	lg, m := newTestLedger(t)
	ctx := context.Background()

	_ = lg.Append(ctx, 1, "payload-1")
	_ = lg.Append(ctx, 2, "payload-2")
	_ = lg.Append(ctx, 3, "payload-3")

	// 篡改第 2 条的 entry_hash（模拟攻击者修改数据库）
	m.mu.Lock()
	m.entries[1].EntryHash = "fakehash123"
	m.mu.Unlock()

	res, err := lg.Verify(ctx)
	if err != nil {
		t.Fatalf("校验出错: %v", err)
	}
	if res.Valid {
		t.Fatal("篡改后账本不应有效")
	}
	found := false
	for _, e := range res.Errors {
		if strings.Contains(e, "哈希链断裂") {
			found = true
		}
	}
	if !found {
		t.Errorf("应报告哈希链断裂，实际错误: %v", res.Errors)
	}
}

// TestTamperSignatureDetected 测试：修改 payload_hash → 签名校验失败
func TestTamperSignatureDetected(t *testing.T) {
	lg, m := newTestLedger(t)
	ctx := context.Background()

	_ = lg.Append(ctx, 1, "payload-1")
	_ = lg.Append(ctx, 2, "payload-2")

	// 篡改 payload_hash（修改内容但保留原签名）
	m.mu.Lock()
	m.entries[0].PayloadHash = "changed"
	m.mu.Unlock()

	res, err := lg.Verify(ctx)
	if err != nil {
		t.Fatalf("校验出错: %v", err)
	}
	if res.Valid {
		t.Fatal("篡改后账本不应有效")
	}
}

// TestSnapshotCreateAndVerify 测试：快照创建 + 锚点校验
func TestSnapshotCreateAndVerify(t *testing.T) {
	lg, m := newTestLedger(t)
	ctx := context.Background()

	_ = lg.Append(ctx, 1, "payload-1")
	_ = lg.Append(ctx, 2, "payload-2")

	// 创建快照
	if err := lg.CreateSnapshot(ctx, "2026-08-08"); err != nil {
		t.Fatalf("创建快照失败: %v", err)
	}

	sn, _ := m.GetLatestSnapshot(ctx)
	if sn == nil {
		t.Fatal("快照未创建")
	}

	// 快照链尾哈希应等于最后一条的 entry_hash
	last, _ := m.GetLastLedgerEntry(ctx)
	if sn.LastHash != last.EntryHash {
		t.Errorf("快照链尾 %q 不等于账本链尾 %q", sn.LastHash, last.EntryHash)
	}

	// 校验应通过且快照锚点匹配
	res, err := lg.Verify(ctx)
	if err != nil {
		t.Fatalf("校验失败: %v", err)
	}
	if !res.Valid || !res.SnapshotOK {
		t.Errorf("校验应有效且快照锚点匹配，valid=%v snapshot_ok=%v", res.Valid, res.SnapshotOK)
	}
}

// TestSnapshotAnchorMismatch 测试：快照锚点不在链上 → 校验失败（真正的数据异常）
func TestSnapshotAnchorMismatch(t *testing.T) {
	lg, m := newTestLedger(t)
	ctx := context.Background()

	_ = lg.Append(ctx, 1, "payload-1")
	_ = lg.Append(ctx, 2, "payload-2")
	if err := lg.CreateSnapshot(ctx, "2026-08-08"); err != nil {
		t.Fatalf("创建快照失败: %v", err)
	}

	// 快照后正常追加新记录：锚点是历史锚点（仍在链上），校验应通过
	_ = lg.Append(ctx, 3, "payload-3")

	res, err := lg.Verify(ctx)
	if err != nil {
		t.Fatalf("校验出错: %v", err)
	}
	if !res.Valid || !res.SnapshotOK {
		t.Errorf("快照锚点在链上（历史锚点）应有效，valid=%v snapshot_ok=%v", res.Valid, res.SnapshotOK)
	}

	// 篡改快照锚点为链上不存在的哈希（模拟数据库被整体替换 / 快照被篡改）
	m.mu.Lock()
	m.snapshots[len(m.snapshots)-1].LastHash = "deadbeef-not-on-chain"
	m.mu.Unlock()

	res, err = lg.Verify(ctx)
	if err != nil {
		t.Fatalf("校验出错: %v", err)
	}
	if res.Valid {
		t.Fatal("快照锚点不在链上时账本不应标记为有效")
	}
	if res.SnapshotOK {
		t.Error("快照锚点不在链上时 SnapshotOK 应为 false")
	}
}

// TestPublicKeyPEM 测试：公钥导出为合法 PEM
func TestPublicKeyPEM(t *testing.T) {
	lg, _ := newTestLedger(t)
	pem := lg.PublicKeyPEM()
	if !strings.Contains(pem, "-----BEGIN PUBLIC KEY-----") {
		t.Errorf("公钥应包含 PEM 头，实际: %q", pem)
	}
	if !strings.Contains(pem, "-----END PUBLIC KEY-----") {
		t.Errorf("公钥应包含 PEM 尾，实际: %q", pem)
	}
}

// TestSignatureUnique 测试：相同内容不同事件 → 不同签名（防重放）
func TestSignatureUnique(t *testing.T) {
	lg, m := newTestLedger(t)
	ctx := context.Background()

	// 相同 payload 但不同 eventID
	_ = lg.Append(ctx, 1, "same-payload")
	_ = lg.Append(ctx, 2, "same-payload")

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries[0].EntryHash == m.entries[1].EntryHash {
		t.Error("不同事件不应有相同 entry_hash（防重放）")
	}
	if m.entries[0].Signature == m.entries[1].Signature {
		t.Error("不同事件不应有相同签名（防重放）")
	}
}
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
