// Package personal 频次风控：滑动窗口限流器（个人免签推送/心跳用）
//
// 设计（docs/MIANQIAN.md §10 风控）：
//   - 频次风控：每分钟收款笔数上限（可配置），超限拒绝 + 告警
//   - 滑动窗口实现：按 key（device_id）维护 60s 内的时间戳列表，O(1) 摊销
//   - 内存占用受控：窗口外旧记录惰性清理，key 总数上限保护
package personal

import (
	"sync"
	"time"
)

// RateLimiter 滑动窗口限流器（线程安全）
type RateLimiter struct {
	mu      sync.Mutex
	limit   int                // 窗口内最大次数（<=0 表示不限）
	window  time.Duration      // 窗口时长（默认 60s）
	maxKeys int                // key 数上限（防内存无限增长，默认 10000）
	hits    map[string][]int64 // key → 窗口内时间戳（Unix 秒）
}

// NewRateLimiter 创建限流器
// limit: 窗口内最大次数；window: 窗口时长；maxKeys: key 数上限（0 用默认 10000）
func NewRateLimiter(limit int, window time.Duration, maxKeys int) *RateLimiter {
	if window <= 0 {
		window = time.Minute
	}
	if maxKeys <= 0 {
		maxKeys = 10000
	}
	return &RateLimiter{
		limit:   limit,
		window:  window,
		maxKeys: maxKeys,
		hits:    make(map[string][]int64),
	}
}

// Allow 判断 key 是否允许本次请求（允许则记录）
func (r *RateLimiter) Allow(key string) bool {
	if r.limit <= 0 {
		return true // 未配置限额
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now().Unix()
	cutoff := now - int64(r.window.Seconds())

	// 惰性清理：移除窗口外记录；空列表直接删 key
	hits := r.hits[key]
	kept := hits[:0]
	for _, t := range hits {
		if t > cutoff {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(r.hits, key)
		kept = nil
	}
	if len(kept) >= r.limit {
		r.hits[key] = kept
		return false
	}
	// key 数上限保护（仅新增 key 时触发）
	if len(kept) == 0 && len(r.hits) >= r.maxKeys {
		// 清掉最久未活动的 key：简单策略，遍历删除首个空窗口 key
		for k, v := range r.hits {
			if len(v) > 0 && v[len(v)-1] <= cutoff {
				delete(r.hits, k)
				break
			}
		}
	}
	r.hits[key] = append(kept, now)
	return true
}

// Count 返回 key 当前窗口内次数（诊断用）
func (r *RateLimiter) Count(key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Unix() - int64(r.window.Seconds())
	n := 0
	for _, t := range r.hits[key] {
		if t > cutoff {
			n++
		}
	}
	return n
}
