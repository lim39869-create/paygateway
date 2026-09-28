// Package upstream 上游对接方管理：多商户（易支付 pid+key）独立加密存储
//
// UpstreamPool 是各模块获取上游数据的唯一入口：
//   - epay 协议层（main.go 验签）：按 pid 查对应密钥
//   - order.Service：下单记录 pid/upstream_name；回调签名按订单 pid 取密钥
//   - Admin API：增删改查后同步到 pool
//
// 设计约束：
//   - 不直接访问 upstreams.db（仅 Admin API 有权限）
//   - 所有密钥在内存中解密，不落盘
package upstream

import (
	"sync"
)

// UpstreamPool 内存上游池（线程安全）
// 负责：pid → Upstream 索引（验签/签名路由）
type UpstreamPool struct {
	mu    sync.RWMutex
	byID  map[int64]*Upstream  // id → Upstream
	byPid map[string]*Upstream // pid → Upstream（易支付验签/签名路由）
}

// NewPool 创建空上游池
func NewPool() *UpstreamPool {
	return &UpstreamPool{
		byID:  make(map[int64]*Upstream),
		byPid: make(map[string]*Upstream),
	}
}

// ---------- 管理接口（仅 Admin API 调用）----------

// Add 添加上游到池中（Admin API 解密后调用）
func (p *UpstreamPool) Add(u *Upstream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.byID[u.ID] = u
	if u.Pid != "" {
		p.byPid[u.Pid] = u
	}
}

// Update 更新池中上游（pid 变更时重建索引）
func (p *UpstreamPool) Update(u *Upstream) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.byID[u.ID]; ok {
		if old.Pid != "" && old.Pid != u.Pid {
			delete(p.byPid, old.Pid)
		}
	}
	p.byID[u.ID] = u
	if u.Pid != "" {
		p.byPid[u.Pid] = u
	}
}

// Remove 从池中移除上游
func (p *UpstreamPool) Remove(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.byID[id]; ok {
		if old.Pid != "" {
			delete(p.byPid, old.Pid)
		}
		delete(p.byID, id)
	}
}

// SetStatus 设置上游状态
func (p *UpstreamPool) SetStatus(id int64, status UpstreamStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if u, ok := p.byID[id]; ok {
		u.Status = status
	}
}

// ---------- 查询接口（业务模块使用）----------

// ByPid 按易支付商户ID查找上游（验签/签名路由）
// 返回 nil 表示该 pid 未登记
func (p *UpstreamPool) ByPid(pid string) *Upstream {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byPid[pid]
}

// ByID 按 ID 查找上游
func (p *UpstreamPool) ByID(id int64) *Upstream {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byID[id]
}

// All 返回全部上游（按 ID 排序，Admin 列表用）
func (p *UpstreamPool) All() []*Upstream {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*Upstream, 0, len(p.byID))
	for _, u := range p.byID {
		out = append(out, u)
	}
	// 按 ID 升序
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Count 上游总数
func (p *UpstreamPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.byID)
}
