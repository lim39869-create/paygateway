// Package account 账户池：内存缓存 + 调度器
//
// AccountPool 是各模块获取账户数据的唯一入口：
//   - alipay.Channel 持有 pool，下单选账户
//   - order.Service 持有 pool，回调反查 app_id
//   - Admin API 持有 pool，增删改查后同步到 pool
//
// 设计约束：
//   - 不直接访问 accounts.db（仅 Admin API 有权限）
//   - 所有账户数据在内存中解密，不落盘
package account

import (
	"math/rand"
	"sync"
	"time"
)

// AccountPool 内存账户池（线程安全）
// 负责：账户选择、日限额检查、风控冷却
type AccountPool struct {
	mu       sync.RWMutex
	accounts map[int64]*Account  // id → Account
	byAppID  map[string]*Account // app_id → Account（支付宝回调反查）
	byMchID  map[string]*Account // mch_id → Account（微信回调反查）
	// offlineFrozen 记录因「设备离线联动」冻结的账户（仅内存，重启后由 Add 依据 FrozenReason 重建）。
	// 自动解冻只能针对此集合——风控冻结/手动冻结的账户绝不允许被设备恢复自动解冻。
	offlineFrozen map[int64]bool
}

// NewPool 创建空账户池
func NewPool() *AccountPool {
	return &AccountPool{
		accounts:      make(map[int64]*Account),
		byAppID:       make(map[string]*Account),
		byMchID:       make(map[string]*Account),
		offlineFrozen: make(map[int64]bool),
	}
}

// FreezeByOffline 离线联动冻结账户（设备 adb 不可达时调用）。
// 记录 FrozenReason=offline + offlineFrozen 标记，设备恢复时允许自动解冻。
func (p *AccountPool) FreezeByOffline(id int64) {
	p.freezeWithReason(id, FrozenReasonOffline)
}

// FreezeByRisk 风控上报冻结账户（仅人工解冻，绝不允许自动解冻）
func (p *AccountPool) FreezeByRisk(id int64) {
	p.freezeWithReason(id, FrozenReasonRisk)
}

// FreezeManual 手动冻结账户（管理员操作，仅人工解冻）
func (p *AccountPool) FreezeManual(id int64) {
	p.freezeWithReason(id, FrozenReasonManual)
}

// FreezeByRetire 删除设备级联停用（僵尸自动账户，仅人工恢复）
func (p *AccountPool) FreezeByRetire(id int64) {
	p.freezeWithReason(id, FrozenReasonRetire)
}

// freezeWithReason 统一冻结入口：记录来源 + 维护 offlineFrozen 标记
func (p *AccountPool) freezeWithReason(id int64, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	acct, ok := p.accounts[id]
	if !ok {
		return
	}
	acct.Status = StatusFrozen
	acct.FrozenReason = reason
	if reason == FrozenReasonOffline {
		p.offlineFrozen[id] = true
	} else {
		delete(p.offlineFrozen, id)
	}
}

// UnfreezeByOffline 仅解冻「离线联动冻结」的账户（设备 adb 恢复时调用）。
// 手动冻结/风控冻结的账户（未在 offlineFrozen 中）不受影响，保持 frozen。
func (p *AccountPool) UnfreezeByOffline(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.offlineFrozen[id] {
		return
	}
	delete(p.offlineFrozen, id)
	if acct, ok := p.accounts[id]; ok {
		acct.Status = StatusNormal
		acct.FrozenReason = ""
	}
}

// IsOfflineFrozen 返回账户是否因「设备离线联动」冻结（仅内部测试/诊断用）
func (p *AccountPool) IsOfflineFrozen(id int64) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.offlineFrozen[id]
}

// ---------- 管理接口（仅 Admin API 调用）----------

// Add 添加账户到池中（Admin API 解密后调用）
func (p *AccountPool) Add(acct *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.accounts[acct.ID] = acct
	if acct.AppID != "" {
		p.byAppID[acct.AppID] = acct
	}
	if acct.MchID != "" {
		p.byMchID[acct.MchID] = acct
	}
	// 重启后重建离线联动冻结标记（FrozenReason 持久化，服务重启自动解冻能力不丢）
	if acct.Status == StatusFrozen && acct.FrozenReason == FrozenReasonOffline {
		p.offlineFrozen[acct.ID] = true
	}
}

// Update 更新池中账户
func (p *AccountPool) Update(acct *Account) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// 移除旧索引
	if old, ok := p.accounts[acct.ID]; ok {
		if old.AppID != "" {
			delete(p.byAppID, old.AppID)
		}
		if old.MchID != "" {
			delete(p.byMchID, old.MchID)
		}
	}
	p.accounts[acct.ID] = acct
	if acct.AppID != "" {
		p.byAppID[acct.AppID] = acct
	}
	if acct.MchID != "" {
		p.byMchID[acct.MchID] = acct
	}
}

// Remove 从池中移除账户
func (p *AccountPool) Remove(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.accounts[id]; ok {
		if old.AppID != "" {
			delete(p.byAppID, old.AppID)
		}
		if old.MchID != "" {
			delete(p.byMchID, old.MchID)
		}
		delete(p.accounts, id)
	}
}

// SetStatus 设置账户状态
func (p *AccountPool) SetStatus(id int64, status AccountStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acct, ok := p.accounts[id]; ok {
		acct.Status = status
	}
}

// ListAll 返回所有账户的快照（管理后台用）
func (p *AccountPool) ListAll() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Account, 0, len(p.accounts))
	for _, acct := range p.accounts {
		// 复制一份，避免外部修改
		c := *acct
		out = append(out, &c)
	}
	return out
}

// ListByChannel 返回某通道所有账户（查单/对账用）
func (p *AccountPool) ListByChannel(channel string) []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []*Account
	for _, acct := range p.accounts {
		if acct.Channel == channel {
			c := *acct
			out = append(out, &c)
		}
	}
	return out
}

// CountByChannel 返回某通道的账户数量
func (p *AccountPool) CountByChannel(channel string) int {
	return len(p.ListByChannel(channel))
}

// GetByID 按 ID 获取账户
func (p *AccountPool) GetByID(id int64) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if acct, ok := p.accounts[id]; ok {
		c := *acct
		return &c
	}
	return nil
}

// ---------- 查询接口（各模块调用）----------

// LookupByAppID 按 app_id 反查账户（支付宝回调用）
func (p *AccountPool) LookupByAppID(appID string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if acct, ok := p.byAppID[appID]; ok {
		c := *acct
		return &c
	}
	return nil
}

// LookupByMchID 按 mch_id 反查账户（微信回调用）
func (p *AccountPool) LookupByMchID(mchID string) *Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if acct, ok := p.byMchID[mchID]; ok {
		c := *acct
		return &c
	}
	return nil
}

// ---------- 选择调度接口 ----------

// Select 选一个可用账户（加权随机 + 日限额 + 冷却检查）
// channel: 通道类型（alipay / wechat）
// group: 业务分组（"default"）
// 返回 nil 表示无可用账户
func (p *AccountPool) Select(channel, group string) *Account {
	// 1. 过滤可用账户
	candidates := p.filterAvailable(channel, group)
	if len(candidates) == 0 {
		return nil
	}

	// 2. 按 priority 分组（高优先级优先）
	groups := groupByPriority(candidates)

	// 3. 从最高优先级开始选
	priorities := sortedDesc(groups)
	for _, pri := range priorities {
		accts := groups[pri]
		if len(accts) == 0 {
			continue
		}
		// 4. 同优先级内加权随机
		selected := weightedRandom(accts)
		if selected != nil {
			return selected
		}
	}

	return nil
}

// SelectFrom 在可用账户 ∩ allowedIDs 中按优先级/权重选取（设备感知路由用）
// allowedIDs 为空 = 等价于 Select（不过滤）；返回 nil 表示无可用账户
func (p *AccountPool) SelectFrom(channel, group string, allowedIDs map[int64]bool) *Account {
	candidates := p.filterAvailable(channel, group)
	if len(allowedIDs) > 0 && len(candidates) > 0 {
		filtered := make([]*Account, 0, len(candidates))
		for _, acct := range candidates {
			if allowedIDs[acct.ID] {
				filtered = append(filtered, acct)
			}
		}
		candidates = filtered
	}
	if len(candidates) == 0 {
		return nil
	}
	groups := groupByPriority(candidates)
	priorities := sortedDesc(groups)
	for _, pri := range priorities {
		if selected := weightedRandom(groups[pri]); selected != nil {
			return selected
		}
	}
	return nil
}

// SelectFromWeighted 在可用账户 ∩ allowedIDs 中按评分函数选最优（均衡分配用）。
// score 返回账户评分（越高越优先）；评分只用于同优先级内排序，优先级分组仍生效。
// allowedIDs 为空 = 不过滤；score 为 nil = 退化为 weightedRandom（等价 SelectFrom）。
// 返回 nil 表示无可用账户。
func (p *AccountPool) SelectFromWeighted(channel, group string, allowedIDs map[int64]bool, score func(acct *Account) float64) *Account {
	candidates := p.filterAvailable(channel, group)
	if len(allowedIDs) > 0 && len(candidates) > 0 {
		filtered := make([]*Account, 0, len(candidates))
		for _, acct := range candidates {
			if allowedIDs[acct.ID] {
				filtered = append(filtered, acct)
			}
		}
		candidates = filtered
	}
	if len(candidates) == 0 {
		return nil
	}
	if score == nil {
		return p.SelectFrom(channel, group, allowedIDs)
	}
	// 按 priority 分组，组内按评分 argmax（评分相等 → 按 ID 升序，确定性）
	groups := groupByPriority(candidates)
	priorities := sortedDesc(groups)
	for _, pri := range priorities {
		accts := groups[pri]
		best := accts[0]
		bestScore := score(best)
		for _, acct := range accts[1:] {
			s := score(acct)
			if s > bestScore || (s == bestScore && acct.ID < best.ID) {
				best, bestScore = acct, s
			}
		}
		if best != nil {
			c := *best
			return &c
		}
	}
	return nil
}

// ReportResult 上报下单结果（成功/失败），用于熔断统计
// 由 channel 在下单后调用
func (p *AccountPool) ReportResult(accountID int64, success bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	acct, ok := p.accounts[accountID]
	if !ok {
		return
	}

	if success {
		// 成功：重置失败计数
		acct.FailCount = 0
	} else {
		// 失败：递增失败计数
		acct.FailCount++
		if acct.AutoBan && acct.FailCount >= 3 {
			// 连续失败 3 次 → 冷却 15 分钟
			acct.Status = StatusCooling
			now := time.Now()
			until := now.Add(15 * time.Minute)
			acct.FrozenUntil = &until
		}
	}
}

// AddDailyUsed 增加日已用额度（下单成功后调用）
func (p *AccountPool) AddDailyUsed(accountID int64, amount int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if acct, ok := p.accounts[accountID]; ok {
		acct.DailyUsed += amount
	}
}

// ResetDailyUsed 重置所有账户的日已用额度（每日 00:00 调用，与 DB 重置同步）
func (p *AccountPool) ResetDailyUsed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, acct := range p.accounts {
		acct.DailyUsed = 0
	}
}

// Count 返回池中账户总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.accounts)
}

// ---------- 内部辅助 ----------

// filterAvailable 过滤可用账户
// 注意：本函数会"恢复过期冷却"（写操作），因此必须持有写锁（由 Select 调用时保证）
func (p *AccountPool) filterAvailable(channel, group string) []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	var out []*Account
	for _, acct := range p.accounts {
		// 通道匹配
		if acct.Channel != channel {
			continue
		}
		// 分组匹配
		if acct.Group != group {
			continue
		}
		// 冷却到期的自动恢复：必须先于状态过滤。
		// 只恢复 cooling（自动熔断到期）；frozen（离线联动/风控/手动）不在此解冻，
		// 由设备心跳恢复联动或管理员操作解除。
		if acct.Status == StatusCooling && acct.FrozenUntil != nil && !acct.FrozenUntil.After(now) {
			acct.Status = StatusNormal
			acct.FrozenUntil = nil
			acct.FailCount = 0
		}
		// 状态检查：仅 normal 可用
		if acct.Status != StatusNormal {
			continue
		}
		// 日限额检查
		if acct.DailyLimit > 0 && acct.DailyUsed >= acct.DailyLimit {
			continue
		}
		// 冷却中
		if acct.FrozenUntil != nil && acct.FrozenUntil.After(now) {
			continue
		}
		out = append(out, acct)
	}
	return out
}

// groupByPriority 按 priority 分组（高→低）
func groupByPriority(accts []*Account) map[int][]*Account {
	groups := make(map[int][]*Account)
	for _, acct := range accts {
		groups[acct.Priority] = append(groups[acct.Priority], acct)
	}
	return groups
}

// sortedDesc 返回排序后的 key 列表（降序）
func sortedDesc(m map[int][]*Account) []int {
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// 简单排序（降序）
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] > keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

// weightedRandom 加权随机选择
func weightedRandom(accts []*Account) *Account {
	if len(accts) == 0 {
		return nil
	}
	if len(accts) == 1 {
		c := *accts[0]
		return &c
	}

	totalWeight := 0
	for _, acct := range accts {
		weight := acct.Weight
		if weight <= 0 {
			weight = 1
		}
		totalWeight += weight
	}

	r := rand.Intn(totalWeight)
	cumulative := 0
	for _, acct := range accts {
		weight := acct.Weight
		if weight <= 0 {
			weight = 1
		}
		cumulative += weight
		if r < cumulative {
			c := *acct
			return &c
		}
	}

	// fallback
	c := *accts[len(accts)-1]
	return &c
}
