// Package ratelimit 提供基于 IP 的滑动窗口速率限制中间件。
//
// 用途：保护公网支付接口（/qr/real、/qr/refresh、/submit.php 等），
// 防止单 IP 刷接口 / 暴力枚举 token。多实例部署需改用 Redis 后端，本实现仅覆盖单进程。
package ratelimit

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// limiter 公网 IP 速率限制器实例。
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	max     int           // 窗口内最大请求数
	window  time.Duration // 滑动窗口长度
}

type bucket struct {
	mu    sync.Mutex
	stamp []time.Time // 窗口内请求时间戳
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{buckets: make(map[string]*bucket), max: max, window: window}
}

// allow 判定是否放行；不放行则返回剩余重试等待秒。
func (l *limiter) allow(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{}
		l.buckets[key] = b
	}
	l.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := now.Add(-l.window)
	keep := b.stamp[:0]
	for _, t := range b.stamp {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	if len(keep) >= l.max {
		b.stamp = keep
		// 计算最早一条何时滑出窗口，作为 retry-after
		retry := int(keep[0].Add(l.window).Sub(now).Seconds())
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	b.stamp = append(keep, now)
	return true, 0
}

// prune 清理无活动桶，避免内存泄漏。
func (l *limiter) prune(now time.Time) {
	cutoff := now.Add(-l.window)
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, b := range l.buckets {
		b.mu.Lock()
		stale := len(b.stamp) == 0 || (len(b.stamp) > 0 && b.stamp[len(b.stamp)-1].Before(cutoff))
		b.mu.Unlock()
		if stale {
			delete(l.buckets, key)
		}
	}
}

// IPRateLimit 返回一个 gin 中间件：按 c.ClientIP() 限流，每 window 最多 max req。
// 触发上限时返回 429 + Retry-After 头。
// 通过 max=0 可禁用（直接 next），便于配置可关闭。
func IPRateLimit(max int, window time.Duration) gin.HandlerFunc {
	if max <= 0 || window <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	l := newLimiter(max, window)
	var pruneCounter int
	return func(c *gin.Context) {
		ip := c.ClientIP()
		now := time.Now()
		// 每 64 请求清理一次过期桶，平摊成本
		pruneCounter++
		if pruneCounter%64 == 0 {
			l.prune(now)
		}
		ok, retry := l.allow(ip, now)
		if !ok {
			c.Header("Retry-After", strconv.Itoa(retry))
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":           "请求过于频繁，请稍后再试",
				"retry_after_sec": retry,
			})
			return
		}
		c.Next()
	}
}
