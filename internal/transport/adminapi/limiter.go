package adminapi

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	loginWindow        = time.Minute
	loginMaxFailures   = 5
	loginBlockFor      = 5 * time.Minute
	ticketWindow       = time.Minute
	ticketMaxPerWindow = 60
	limiterMaxEntries  = 10000
)

type loginAttempt struct {
	failures     int
	windowStart  time.Time
	blockedUntil time.Time
}

// loginLimiter 是进程内的登录失败限速器，按「客户端 IP + 用户名」计数。
// 本项目是单节点原型；多节点部署必须换成共享存储（如 Redis）才能真正防暴力破解。
type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginAttempt
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]*loginAttempt)}
}

// blocked 返回该 key 仍需等待的时间，零表示可以继续尝试。
func (l *loginLimiter) blocked(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(now)
	entry := l.entries[key]
	if entry == nil {
		return 0
	}
	if remain := entry.blockedUntil.Sub(now); remain > 0 {
		return remain
	}
	return 0
}

// recordFailure 累计一次失败；达到阈值后锁定该 key 一段时间。
// 返回 false 表示计数表已满、无法记录，调用方必须直接拒绝本次请求，
// 否则攻击者可以用大量新 key 绕过限速。
func (l *loginLimiter) recordFailure(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry := l.entries[key]
	if entry == nil {
		if len(l.entries) >= limiterMaxEntries {
			l.prune(now)
			if len(l.entries) >= limiterMaxEntries {
				return false
			}
		}
		entry = &loginAttempt{windowStart: now}
		l.entries[key] = entry
	}
	if now.Sub(entry.windowStart) >= loginWindow {
		entry.windowStart, entry.failures = now, 0
	}
	entry.failures++
	if entry.failures >= loginMaxFailures {
		entry.blockedUntil = now.Add(loginBlockFor)
		entry.failures = 0
	}
	return true
}

func (l *loginLimiter) recordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// prune 清理已过期且未锁定的记录；调用方必须持有 mu。
func (l *loginLimiter) prune(now time.Time) {
	for key, entry := range l.entries {
		if !entry.blockedUntil.After(now) && now.Sub(entry.windowStart) >= loginWindow {
			delete(l.entries, key)
		}
	}
}

type windowCount struct {
	count int
	start time.Time
}

// fixedWindowLimiter 用于 ticket 等高频端点的固定窗口限速。
type fixedWindowLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	entries map[string]windowCount
}

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return &fixedWindowLimiter{
		limit:   limit,
		window:  window,
		entries: make(map[string]windowCount),
	}
}

func (l *fixedWindowLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.entries) >= limiterMaxEntries {
		for k, entry := range l.entries {
			if now.Sub(entry.start) >= l.window {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= limiterMaxEntries {
			return false
		}
	}
	entry := l.entries[key]
	if now.Sub(entry.start) >= l.window {
		entry = windowCount{start: now}
	}
	entry.count++
	l.entries[key] = entry
	return entry.count <= l.limit
}

// clientKey 从 RemoteAddr 提取客户端 IP。
// 刻意不信任 X-Forwarded-For：只有明确部署了可信反向代理时才应改用它。
func clientKey(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return strings.ToLower(host)
}
