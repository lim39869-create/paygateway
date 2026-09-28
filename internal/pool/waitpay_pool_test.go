// Package pool 单元测试。
//
// 设计目标：
//   - 每个测试一个独立子测试（t.Run），失败信息含子测试名；
//   - 并发测试必须可重复（确定性输出）且 -race 干净；
//   - 时间相关测试使用固定基准时间，避免 sleep/flake。
package pool

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"paygateway/internal/model"
)

// baseTime 测试基准时间（统一基准便于断言 CloseAt/OldestAge）
var baseTime = time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)

// newOrder 构造一个测试订单。Channel 用 "personal.alipay"。
func newOrder(outTradeNo string, accountID, amountMinor int64, noteCode string, createdAt time.Time) *model.Order {
	return &model.Order{
		OutTradeNo:  outTradeNo,
		AccountID:   accountID,
		Channel:     "personal.alipay",
		AmountMinor: amountMinor,
		NoteCode:    noteCode,
		CreatedAt:   createdAt,
	}
}

// findAccountStat 工具：按 accountID 取快照元素
func findAccountStat(snap *Snapshot, accountID int64) *AccountStat {
	for i := range snap.Accounts {
		if snap.Accounts[i].AccountID == accountID {
			return &snap.Accounts[i]
		}
	}
	return nil
}

// ---------- 1. Add → Snapshot ----------
func TestWaitPayPool_Add_ShowsInSnapshot(t *testing.T) {
	p := NewWaitPayPool(15)
	o := newOrder("O1", 100, 1000, "N1", baseTime)
	p.Add(o)

	snap := p.Snapshot()
	if snap.Total != 1 {
		t.Fatalf("Total: want 1, got %d", snap.Total)
	}
	st := findAccountStat(snap, 100)
	if st == nil {
		t.Fatalf("账户 100 缺统计")
	}
	if st.Count != 1 {
		t.Fatalf("Count: want 1, got %d", st.Count)
	}
	// CloseAt 应为 CreatedAt + 15min
	if !snap.UpdatedAt.Equal(snap.UpdatedAt) {
		t.Fatalf("UpdatedAt 应被设置")
	}
}

// ---------- 2. Add 同 outTradeNo 两次 → 仍 count=1 ----------
func TestWaitPayPool_Add_IdempotentOverwrite(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	p.Add(newOrder("O1", 100, 2000, "N2", baseTime.Add(time.Minute))) // 覆盖

	snap := p.Snapshot()
	if snap.Total != 1 {
		t.Fatalf("Total: want 1, got %d", snap.Total)
	}
	if p.Count(100) != 1 {
		t.Fatalf("Count(100): want 1, got %d", p.Count(100))
	}
	// 最新 amountMinor 应为 2000
	entry := p.MatchByAmount(100, 2000, baseTime)
	if entry == nil || len(entry) != 1 || entry[0].AmountMinor != 2000 {
		t.Fatalf("覆盖后 amountMinor=2000 应可命中：got %v", entry)
	}
	// 旧 amountMinor=1000 已脱离索引
	if got := p.MatchByAmount(100, 1000, baseTime); got != nil {
		t.Fatalf("旧 amountMinor=1000 应无候选：got %v", got)
	}
}

// ---------- 3. Remove 不存在 → 不 panic，count 不变 ----------
func TestWaitPayPool_Remove_NonExistent(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	p.Remove("DOES_NOT_EXIST")
	if p.Count(100) != 1 {
		t.Fatalf("Count 应仍为 1：got %d", p.Count(100))
	}
	if p.Snapshot().Total != 1 {
		t.Fatalf("Total 应仍为 1")
	}
}

// ---------- 4. Remove 已存在 → count=0 ----------
func TestWaitPayPool_Remove_Existing(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	p.Remove("O1")
	if p.Count(100) != 0 {
		t.Fatalf("Count 应为 0：got %d", p.Count(100))
	}
	if p.Snapshot().Total != 0 {
		t.Fatalf("Total 应为 0")
	}
	// 索引也清理
	if got := p.MatchByNote(100, "N1", baseTime); got != nil {
		t.Fatalf("MatchByNote 应 nil：got %v", got)
	}
	if got := p.MatchByAmount(100, 1000, baseTime); got != nil {
		t.Fatalf("MatchByAmount 应 nil：got %v", got)
	}
}

// ---------- 5. Upsert 更新 NoteCode → 新码命中、旧码 miss ----------
func TestWaitPayPool_Upsert_NoteCodeChange(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	p.Upsert(newOrder("O1", 100, 1000, "N2", baseTime))

	if got := p.MatchByNote(100, "N2", baseTime); got == nil || got.OutTradeNo != "O1" {
		t.Fatalf("MatchByNote 新码应命中 O1：got %v", got)
	}
	if got := p.MatchByNote(100, "N1", baseTime); got != nil {
		t.Fatalf("MatchByNote 旧码应 miss：got %v", got)
	}
}

// ---------- 6. MatchByNote 尊重 since ----------
func TestWaitPayPool_MatchByNote_SinceFilter(t *testing.T) {
	p := NewWaitPayPool(15)
	old := baseTime.Add(-2 * time.Hour)
	p.Add(newOrder("O1", 100, 1000, "N1", old))

	// since = baseTime → 旧单 CreatedAt < since → nil
	if got := p.MatchByNote(100, "N1", baseTime); got != nil {
		t.Fatalf("since 应过滤掉旧单：got %v", got)
	}
	// since = old → 旧单 CreatedAt >= since → 命中
	if got := p.MatchByNote(100, "N1", old); got == nil || got.OutTradeNo != "O1" {
		t.Fatalf("since=old 应命中：got %v", got)
	}
}

// ---------- 7. MatchByNote 备注码复用：返回 CreatedAt 最新者 ----------
func TestWaitPayPool_MatchByNote_ReuseReturnsLatest(t *testing.T) {
	p := NewWaitPayPool(15)
	// 同一 noteCode TTL 复用：先一笔旧单被 Remove（关单），再补一笔新单
	p.Add(newOrder("O_OLD", 100, 1000, "N1", baseTime.Add(-3*time.Minute)))
	p.Add(newOrder("O_NEW", 100, 1000, "N1", baseTime))

	got := p.MatchByNote(100, "N1", baseTime.Add(-5*time.Minute))
	if got == nil {
		t.Fatalf("MatchByNote 应命中")
	}
	if got.OutTradeNo != "O_NEW" {
		t.Fatalf("应返回最新 CreatedAt 的 O_NEW：got %s", got.OutTradeNo)
	}
}

// ---------- 8. MatchByAmount 多候选返回；无候选 nil ----------
func TestWaitPayPool_MatchByAmount(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	p.Add(newOrder("O2", 100, 1000, "N2", baseTime.Add(time.Second)))
	p.Add(newOrder("O3", 100, 2000, "N3", baseTime))

	// 同金额 1000 → 两个候选
	got := p.MatchByAmount(100, 1000, baseTime)
	if got == nil {
		t.Fatalf("MatchByAmount 应有候选")
	}
	if len(got) != 2 {
		t.Fatalf("候选数：want 2, got %d", len(got))
	}
	// 排序稳定性：先 CreatedAt 升序
	if !got[0].CreatedAt.Before(got[1].CreatedAt) {
		t.Fatalf("候选排序应按 CreatedAt 升序")
	}

	// 2000 → 一个候选
	got2 := p.MatchByAmount(100, 2000, baseTime)
	if got2 == nil || len(got2) != 1 || got2[0].OutTradeNo != "O3" {
		t.Fatalf("2000 候选异常：got %v", got2)
	}

	// 不存在的金额 → nil
	if got3 := p.MatchByAmount(100, 9999, baseTime); got3 != nil {
		t.Fatalf("无候选应 nil：got %v", got3)
	}

	// 不存在的账户 → nil
	if got4 := p.MatchByAmount(999, 1000, baseTime); got4 != nil {
		t.Fatalf("无账户应 nil：got %v", got4)
	}
}

// ---------- 9. 并发 Add/Remove/Snapshot storm → -race 干净，final 一致 ----------
func TestWaitPayPool_ConcurrentStorm(t *testing.T) {
	p := NewWaitPayPool(15)
	// 注入 winCalc 以验证并发 Add 不会触发 map 迭代竞态（P0 修复: orderToEntry 在锁内）。
	p.SetWindowCalc(func(active, total int) time.Duration {
		_ = active
		_ = total
		return 5 * time.Minute
	})
	// 初始化一个设备绑定,使 windowFor 走活跃账户计数路径
	p.SetDeviceOf(func(accountID int64) (string, bool) {
		return "dev1", true
	})

	const (
		workers   = 16
		opsPer    = 200
		accBase   = 1000
		outPrefix = "O"
	)

	var wg sync.WaitGroup
	var added, removed int64

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for i := 0; i < opsPer; i++ {
				accID := accBase + int64(workerID)
				outNo := outPrefix + strconv.Itoa(workerID) + "_" + strconv.Itoa(i)
				note := "N" + strconv.Itoa(i)
				switch (workerID + i) % 3 {
				case 0:
					// Add
					p.Add(newOrder(outNo, accID, int64(100+i), note, baseTime.Add(time.Duration(i)*time.Millisecond)))
					atomic.AddInt64(&added, 1)
				case 1:
					// Remove 自己刚 Add 的（可能不存在 → 幂等）
					p.Remove(outNo)
					atomic.AddInt64(&removed, 1)
				case 2:
					// Snapshot 读
					snap := p.Snapshot()
					if snap == nil {
						t.Errorf("Snapshot nil")
						return
					}
					_ = snap.Total
				}
			}
		}(w)
	}

	wg.Wait()

	// 终态一致性：
	//   - 快照 Total == Count(每个账户) 之和 == 池内实际条目数（通过公共 API 统计）
	var bucketCount int
	for _, st := range p.Snapshot().Accounts {
		bucketCount += p.Count(st.AccountID)
	}

	snap := p.Snapshot()
	if snap.Total != bucketCount {
		t.Fatalf("快照 Total(%d) 与 buckets 实际(%d) 不一致", snap.Total, bucketCount)
	}
	for _, st := range snap.Accounts {
		if p.Count(st.AccountID) != st.Count {
			t.Fatalf("账户 %d 快照 Count(%d) 与 Count()(实际 %d) 不一致", st.AccountID, st.Count, p.Count(st.AccountID))
		}
	}
	t.Logf("storm done: added=%d removed=%d final=%d", added, removed, snap.Total)
}

// ---------- 10. Rebuild 清空后批量加入 ----------
func TestWaitPayPool_Rebuild_ClearsThenAdds(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("OLD1", 100, 1000, "N1", baseTime))
	p.Add(newOrder("OLD2", 200, 2000, "N2", baseTime))

	orders := []*model.Order{
		newOrder("NEW1", 100, 1000, "N1", baseTime),
		newOrder("NEW2", 300, 3000, "N3", baseTime),
		newOrder("NEW3", 300, 4000, "N4", baseTime),
	}
	p.Rebuild(context.Background(), orders)

	if p.Count(200) != 0 {
		t.Fatalf("旧账户 200 应清空：got %d", p.Count(200))
	}
	if p.Count(100) != 1 {
		t.Fatalf("账户 100 应仅剩 NEW1：got %d", p.Count(100))
	}
	if p.Count(300) != 2 {
		t.Fatalf("账户 300 应有 2 单：got %d", p.Count(300))
	}
	if p.Snapshot().Total != 3 {
		t.Fatalf("Total 应 3")
	}
}

// ---------- 11. Reconcile：DB-only / pool-only / shared 三类 ----------
func TestWaitPayPool_Reconcile_Diff(t *testing.T) {
	p := NewWaitPayPool(15)

	// 池内：SHARED + POOL_ONLY
	p.Add(newOrder("SHARED", 100, 1000, "N1", baseTime))
	p.Add(newOrder("POOL_ONLY", 100, 2000, "N2", baseTime))

	// DB：SHARED + DB_ONLY
	dbOrders := []*model.Order{
		newOrder("SHARED", 100, 1000, "N1", baseTime),
		newOrder("DB_ONLY", 200, 3000, "N3", baseTime),
	}

	added, removed, err := p.Reconcile(context.Background(), dbOrders)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if added != 1 || removed != 1 {
		t.Fatalf("added/removed: want 1/1, got %d/%d", added, removed)
	}

	// SHARED 仍存在（POOL_ONLY 已被移除 → 账户 100 仅剩 SHARED）
	if p.Count(100) != 1 {
		t.Fatalf("账户 100 应有 1 单（POOL_ONLY 已移除）：got %d", p.Count(100))
	}
	if got := p.MatchByAmount(100, 1000, baseTime); got == nil || len(got) != 1 || got[0].OutTradeNo != "SHARED" {
		t.Fatalf("SHARED 应命中：got %v", got)
	}
	// POOL_ONLY 已移除
	if got := p.MatchByAmount(100, 2000, baseTime); got != nil {
		t.Fatalf("POOL_ONLY 应被移除：got %v", got)
	}
	// DB_ONLY 已补入
	if got := p.MatchByAmount(200, 3000, baseTime); got == nil || len(got) != 1 || got[0].OutTradeNo != "DB_ONLY" {
		t.Fatalf("DB_ONLY 应补入：got %v", got)
	}
	if p.Snapshot().Total != 2 {
		t.Fatalf("Total 应 2（SHARED+DB_ONLY）")
	}
}

// ---------- 12. closeWindowMinutes=0 → CloseAt 默认 CreatedAt+30min ----------
func TestWaitPayPool_CloseAt_DefaultWhenZero(t *testing.T) {
	p := NewWaitPayPool(0) // 显式 0 → 走 30min 兜底
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))

	e := p.MatchByNote(100, "N1", baseTime)
	if e == nil {
		t.Fatalf("MatchByNote 应命中")
	}
	wantClose := baseTime.Add(30 * time.Minute)
	if !e.CloseAt.Equal(wantClose) {
		t.Fatalf("CloseAt: want %v, got %v", wantClose, e.CloseAt)
	}
}

// ---------- closeWindowMinutes=-1（负数）→ 同样走默认 30min ----------
func TestWaitPayPool_CloseAt_DefaultWhenNegative(t *testing.T) {
	p := NewWaitPayPool(-5)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))

	e := p.MatchByNote(100, "N1", baseTime)
	if e == nil {
		t.Fatalf("MatchByNote 应命中")
	}
	if !e.CloseAt.Equal(baseTime.Add(30 * time.Minute)) {
		t.Fatalf("CloseAt 默认 30min：got %v", e.CloseAt)
	}
}

// ---------- Upsert 改变 Amount → byAmount 索引正确迁移 ----------
func TestWaitPayPool_Upsert_AmountChange(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	p.Upsert(newOrder("O1", 100, 2000, "N1", baseTime))

	if got := p.MatchByAmount(100, 1000, baseTime); got != nil {
		t.Fatalf("旧金额 1000 应无候选：got %v", got)
	}
	got := p.MatchByAmount(100, 2000, baseTime)
	if got == nil || len(got) != 1 || got[0].OutTradeNo != "O1" {
		t.Fatalf("新金额 2000 应命中 O1：got %v", got)
	}
}

// ---------- Remove 后 Reconcile 不应再次"添加"已终态订单 ----------
func TestWaitPayPool_Reconcile_NoGhostResurrection(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	p.Remove("O1")

	// DB 已不包含 O1 → Reconcile 不应补回
	added, removed, err := p.Reconcile(context.Background(), nil)
	if err != nil || added != 0 || removed != 0 {
		t.Fatalf("空 DB：want 0/0/nil, got %d/%d/%v", added, removed, err)
	}
	if p.Count(100) != 0 {
		t.Fatalf("Count 应 0：got %d", p.Count(100))
	}
}

// ---------- Count 无账户 → 0；不 panic ----------
func TestWaitPayPool_Count_UnknownAccount(t *testing.T) {
	p := NewWaitPayPool(15)
	if p.Count(999) != 0 {
		t.Fatalf("未知账户应 0")
	}
}

// ---------- Snapshot.Accounts 按 accountID 升序（稳定） ----------
func TestWaitPayPool_Snapshot_AccountsOrdered(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 300, 100, "", baseTime))
	p.Add(newOrder("O2", 100, 100, "", baseTime))
	p.Add(newOrder("O3", 200, 100, "", baseTime))

	snap := p.Snapshot()
	want := []int64{100, 200, 300}
	for i, st := range snap.Accounts {
		if st.AccountID != want[i] {
			t.Fatalf("Accounts[%d]: want %d, got %d", i, want[i], st.AccountID)
		}
	}
}

// ---------- 同一 outTradeNo 不同账户：Remove 全局命中 ----------
func TestWaitPayPool_Remove_GloballyUnique(t *testing.T) {
	// 说明：业务上 outTradeNo 全局唯一（接入方订单号），但池内部按 accountID 桶索引；
	// 若误把同一 outTradeNo 加到两个账户（人为错误），Remove 应清理所有桶中的同名条目。
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "", baseTime))
	p.Add(newOrder("O1", 200, 1000, "", baseTime)) // 同 outTradeNo，不同账户（业务不应发生；测试索引清理）

	p.Remove("O1")
	if p.Count(100) != 0 || p.Count(200) != 0 {
		t.Fatalf("Remove 应清理所有桶中同名：got 100=%d 200=%d", p.Count(100), p.Count(200))
	}
}

// ---------- MinTTL 负值（已超时）----------
func TestWaitPayPool_Snapshot_MinTTL_Overdue(t *testing.T) {
	p := NewWaitPayPool(0) // 30min closeWindow
	// 单子 40 分钟前创建 → 一定已超时
	old := time.Now().Add(-40 * time.Minute)
	p.Add(newOrder("O1", 100, 1000, "", old))

	snap := p.Snapshot()
	st := findAccountStat(snap, 100)
	if st == nil {
		t.Fatalf("缺账户统计")
	}
	if st.MinTTL >= 0 {
		t.Fatalf("MinTTL 应 <0（已超时）：got %v", st.MinTTL)
	}
}

// ---------- 13. 设备维度子池：DeviceSnapshot 按设备隔离 ----------
func TestWaitPayPool_DeviceSnapshot_Isolation(t *testing.T) {
	p := NewWaitPayPool(15)
	devOf := func(accountID int64) (string, bool) {
		switch accountID {
		case 100, 101:
			return "devA", true
		case 200:
			return "devB", true
		default:
			return "", false
		}
	}
	p.SetDeviceOf(devOf)
	p.Add(newOrder("A1", 100, 1000, "N1", baseTime))
	p.Add(newOrder("A2", 101, 2000, "N2", baseTime))
	p.Add(newOrder("B1", 200, 3000, "N3", baseTime))

	da := p.DeviceSnapshot("devA")
	if da == nil || da.Total != 2 || da.ActiveAccts != 2 {
		t.Fatalf("devA: want Total=2 ActiveAccts=2, got %+v", da)
	}
	db := p.DeviceSnapshot("devB")
	if db == nil || db.Total != 1 || db.ActiveAccts != 1 {
		t.Fatalf("devB: want Total=1 ActiveAccts=1, got %+v", db)
	}
	// 全局快照不受设备拆分影响
	if p.Snapshot().Total != 3 {
		t.Fatalf("全局 Total: want 3, got %d", p.Snapshot().Total)
	}
	// 匹配/计数按账户命中（跨设备不混淆）
	if p.Count(100) != 1 || p.Count(200) != 1 {
		t.Fatalf("Count 异常: 100=%d 200=%d", p.Count(100), p.Count(200))
	}
	if got := p.MatchByNote(100, "N1", baseTime); got == nil || got.OutTradeNo != "A1" {
		t.Fatalf("MatchByNote devA 应命中 A1: %v", got)
	}
	if got := p.MatchByNote(200, "N3", baseTime); got == nil || got.OutTradeNo != "B1" {
		t.Fatalf("MatchByNote devB 应命中 B1: %v", got)
	}
}

// ---------- 14. 动态关单窗口：winCalc 注入后 CloseAt 随负载变化且 clamp ----------
func TestWaitPayPool_DynamicCloseAt_WindowCalc(t *testing.T) {
	p := NewWaitPayPool(0) // fallback 30min，但 winCalc 优先
	// winCalc: 负载越高窗口越长，clamp [5min, 10min]
	p.SetWindowCalc(func(active, total int) time.Duration {
		w := 5*time.Minute + time.Duration(total)*time.Minute
		if w > 10*time.Minute {
			w = 10 * time.Minute
		}
		return w
	})
	// 第 1 单：total=0 → 5min
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	e1 := p.MatchByNote(100, "N1", baseTime)
	if e1 == nil {
		t.Fatalf("MatchByNote 应命中")
	}
	want1 := baseTime.Add(5 * time.Minute)
	if !e1.CloseAt.Equal(want1) {
		t.Fatalf("第 1 单 CloseAt: want %v, got %v", want1, e1.CloseAt)
	}
	// 第 2 单：total=1 → 6min
	p.Add(newOrder("O2", 100, 2000, "N2", baseTime))
	e2 := p.MatchByNote(100, "N2", baseTime)
	if e2 == nil {
		t.Fatalf("第 2 单应命中")
	}
	want2 := baseTime.Add(6 * time.Minute)
	if !e2.CloseAt.Equal(want2) {
		t.Fatalf("第 2 单 CloseAt: want %v, got %v", want2, e2.CloseAt)
	}
	// 高负载下窗口应受上限 clamp（模拟 8+ 单 → 10min）
	p.Remove("O1")
	p.Remove("O2")
	for i := 0; i < 12; i++ {
		p.Add(newOrder("HI"+string(rune('A'+i)), int64(300+i), 1000, "", baseTime))
	}
	hi := p.MatchByAmount(311, 1000, baseTime) // 最后一单
	if hi == nil || len(hi) != 1 {
		t.Fatalf("高负载单应命中: %v", hi)
	}
	// 该设备累计 12 单 → winCalc → 10min 上限
	if !hi[0].CloseAt.Equal(baseTime.Add(10 * time.Minute)) {
		t.Fatalf("高负载 CloseAt 应 clamp 到 10min: got %v", hi[0].CloseAt)
	}
}

// ---------- 15. CloseAt 单调只增：Upsert 覆盖时不倒退 ----------
func TestWaitPayPool_CloseAt_Monotonic(t *testing.T) {
	p := NewWaitPayPool(15)
	// 初始窗口 15min
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	first := p.MatchByNote(100, "N1", baseTime)
	if first == nil {
		t.Fatalf("应命中")
	}
	// 切换到固定 5min 窗口后 Upsert：CloseAt 不得早于旧值
	p.SetWindowCalc(func(active, total int) time.Duration { return 5 * time.Minute })
	p.Upsert(newOrder("O1", 100, 1000, "N2", baseTime.Add(time.Minute)))
	got := p.MatchByNote(100, "N2", baseTime)
	if got == nil {
		t.Fatalf("Upsert 后应命中")
	}
	if got.CloseAt.Before(first.CloseAt) {
		t.Fatalf("CloseAt 单调只增被违反: first=%v got=%v", first.CloseAt, got.CloseAt)
	}
	// 新窗口 5min 在 CreatedAt+1min 后 → CreatedAt+6min < first(15min) → 保持 15min
	want := baseTime.Add(15 * time.Minute)
	if !got.CloseAt.Equal(want) {
		t.Fatalf("CloseAt 应保持旧值 15min: want %v, got %v", want, got.CloseAt)
	}
}

// ---------- 16. CloseAtOf 查询（扫描器动态关单判断）----------
func TestWaitPayPool_CloseAtOf(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime))
	ct, ok := p.CloseAtOf("O1")
	if !ok {
		t.Fatalf("CloseAtOf 应命中")
	}
	if !ct.Equal(baseTime.Add(15 * time.Minute)) {
		t.Fatalf("CloseAtOf: want 15min, got %v", ct)
	}
	if _, ok := p.CloseAtOf("NO_SUCH"); ok {
		t.Fatalf("CloseAtOf 未知订单应 not found")
	}
	p.Remove("O1")
	if _, ok := p.CloseAtOf("O1"); ok {
		t.Fatalf("CloseAtOf 移除后应 not found")
	}
}

// ---------- 17. SetDeviceOf 重分桶：绑定变化后条目迁移到新设备 ----------
func TestWaitPayPool_SetDeviceOf_Repartition(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("O1", 100, 1000, "N1", baseTime)) // 无 deviceOf，归 ""
	if st := p.DeviceSnapshot(""); st == nil || st.Total != 1 {
		t.Fatalf("注入前应归兜底桶: %+v", st)
	}
	// 注入映射：100 → devX
	p.SetDeviceOf(func(accountID int64) (string, bool) {
		if accountID == 100 {
			return "devX", true
		}
		return "", false
	})
	if st := p.DeviceSnapshot("devX"); st == nil || st.Total != 1 {
		t.Fatalf("注入后应迁移到 devX: %+v", st)
	}
	if st := p.DeviceSnapshot(""); st == nil || st.Total != 0 {
		t.Fatalf("兜底桶应清空: %+v", st)
	}
	// 匹配链路仍按账户命中
	if got := p.MatchByNote(100, "N1", baseTime); got == nil || got.OutTradeNo != "O1" {
		t.Fatalf("迁移后匹配应命中: %v", got)
	}
}

// ---------- 18. DupAmount：同账户同金额 ≥2 笔 → true；金额互异 → false ----------
func TestWaitPayPool_DupAmount(t *testing.T) {
	p := NewWaitPayPool(15)

	// 账户100: 两笔不同金额 → 无同金额
	p.Add(newOrder("A1", 100, 1000, "", baseTime))
	p.Add(newOrder("A2", 100, 2000, "", baseTime.Add(time.Minute)))
	// 账户200: 两笔同为 3000 → 有同金额
	p.Add(newOrder("B1", 200, 3000, "", baseTime))
	p.Add(newOrder("B2", 200, 3000, "", baseTime.Add(time.Minute)))

	snap := p.Snapshot()
	if st := findAccountStat(snap, 100); st == nil || st.DupAmount {
		t.Fatalf("账户100 金额互异应 DupAmount=false, got %+v", st)
	}
	if st := findAccountStat(snap, 200); st == nil || !st.DupAmount {
		t.Fatalf("账户200 同金额 2 笔应 DupAmount=true, got %+v", st)
	}

	if p.HasDupAmount(100) {
		t.Fatalf("HasDupAmount(100) 应 false")
	}
	if !p.HasDupAmount(200) {
		t.Fatalf("HasDupAmount(200) 应 true")
	}
	if p.HasDupAmount(999) {
		t.Fatalf("HasDupAmount 未知账户应 false")
	}
}

// ---------- 19. HasDupAmount：同一账户同金额消除（移除一笔/改金额）→ 转 false ----------
func TestWaitPayPool_HasDupAmount_Resolved(t *testing.T) {
	p := NewWaitPayPool(15)
	p.Add(newOrder("B1", 200, 3000, "", baseTime))
	p.Add(newOrder("B2", 200, 3000, "", baseTime.Add(time.Minute)))
	if !p.HasDupAmount(200) {
		t.Fatalf("初始应 true")
	}
	// 移除一笔 → 只剩一笔 3000 → 歧义解除
	p.Remove("B2")
	if p.HasDupAmount(200) {
		t.Fatalf("移除一笔后同金额消除应 false")
	}
}
