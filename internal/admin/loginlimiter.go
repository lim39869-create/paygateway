package admin

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"paygateway/internal/logger"
)

// 登录失败退避限速参数。
const (
	loginMaxFailures = 5                // 窗口内失败次数上限，达到即锁定
	loginWindow      = 15 * time.Minute // 滑动窗口长度，同时作为锁定时长
)

// loginAttempt 单条失败记录的时间戳。
type loginAttempt struct {
	ts time.Time
}

// loginBucket 单个键（IP 或 username）的失败记录桶。
type loginBucket struct {
	mu          sync.Mutex
	failures    []time.Time // 滑动窗口内的失败时间戳
	lockedUntil time.Time   // 锁定截止时间，零值表示未锁定
}

// loginLimiter 全局登录失败退避限速器。
// 采用 (IP, username) 双维度独立计数：单 IP 撞多个 username 会被锁，单 username 被多 IP 撞也会被锁。
// 设计目的：阻止暴力破解与用户名枚举。多实例部署需在外层（如 nginx）共享状态，本实现只覆盖单进程场景。
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*loginBucket
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{buckets: make(map[string]*loginBucket)}
}

// bucket 取或建桶。调用方负责加锁外层。
func (l *loginLimiter) bucket(key string) *loginBucket {
	b, ok := l.buckets[key]
	if !ok {
		b = &loginBucket{}
		l.buckets[key] = b
	}
	return b
}

// prune 删除超过 loginWindow 未活动的桶，避免长期泄漏内存。
func (l *loginLimiter) prune(now time.Time) {
	cutoff := now.Add(-loginWindow)
	for key, b := range l.buckets {
		b.mu.Lock()
		stale := len(b.failures) == 0 && (b.lockedUntil.IsZero() || b.lockedUntil.Before(cutoff))
		b.mu.Unlock()
		if stale {
			delete(l.buckets, key)
		}
	}
}

// checkLocked 返回 (是否锁定, 锁定剩余秒)。
func (l *loginLimiter) checkLocked(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	b := l.bucket(key)
	l.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lockedUntil.IsZero() || b.lockedUntil.Before(now) {
		return false, 0
	}
	return true, int(b.lockedUntil.Sub(now).Seconds())
}

// recordFailure 记录一次失败，超过阈值则置锁定。
func (l *loginLimiter) recordFailure(key string, now time.Time) {
	l.mu.Lock()
	if now.IsZero() || len(l.buckets)%50 == 0 {
		l.prune(now)
	}
	b := l.bucket(key)
	l.mu.Unlock()

	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := now.Add(-loginWindow)
	keep := b.failures[:0]
	for _, t := range b.failures {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	b.failures = append(keep, now)
	if len(b.failures) >= loginMaxFailures {
		b.lockedUntil = now.Add(loginWindow)
		logger.Warn("admin", "登录失败次数达上限，已锁定", "key", key, "failures", len(b.failures), "lock_sec", int(loginWindow.Seconds()))
	}
}

// recordSuccess 清零该键的失败记录与锁定状态。
func (l *loginLimiter) recordSuccess(key string) {
	l.mu.Lock()
	b, ok := l.buckets[key]
	l.mu.Unlock()
	if !ok {
		return
	}
	b.mu.Lock()
	b.failures = nil
	b.lockedUntil = time.Time{}
	b.mu.Unlock()
}

// enforceLoginLimit 在 handleLogin 入口调用。
// 返回 true 表示已被限速、调用方应直接 return（已写 429 响应）。
// ipKey / userKey 各自独立触发，取较严格的判定。
func (s *Server) enforceLoginLimit(c *gin.Context, username string) bool {
	now := time.Now()
	ip := c.ClientIP()

	ipKey := "ip:" + ip
	if locked, remain := s.loginLimiter.checkLocked(ipKey, now); locked {
		logger.Warn("admin", "登录被限速（IP 锁定中）", "ip", ip, "remain_sec", remain)
		c.Header("Retry-After", itoa(remain))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "登录尝试过于频繁，请稍后再试", "retry_after_sec": remain})
		return true
	}
	userKey := "user:" + username
	if username != "" {
		if locked, remain := s.loginLimiter.checkLocked(userKey, now); locked {
			logger.Warn("admin", "登录被限速（用户名锁定中）", "username", username, "remain_sec", remain)
			c.Header("Retry-After", itoa(remain))
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "该账号尝试过于频繁，请稍后再试", "retry_after_sec": remain})
			return true
		}
	}
	return false
}

// recordLoginOutcome 在 handleLogin 内判定登录结果后调用，
// failure=true 记录失败，failure=false 清零计数。
// ip 与 username 双维度同步，避免单维度绕过。
func (s *Server) recordLoginOutcome(c *gin.Context, username string, failure bool) {
	now := time.Now()
	ip := c.ClientIP()
	if failure {
		s.loginLimiter.recordFailure("ip:"+ip, now)
		if username != "" {
			s.loginLimiter.recordFailure("user:"+username, now)
		}
		return
	}
	s.loginLimiter.recordSuccess("ip:" + ip)
	if username != "" {
		s.loginLimiter.recordSuccess("user:" + username)
	}
}

// itoa 简单十进制转字符串，避免引入 strconv 只为一处。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
