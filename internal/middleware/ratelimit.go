package middleware

import (
	"context"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"github.com/Sader-Lee/reverse-proxy/internal/config"
)

const (
	// cleanupInterval 是回收空闲令牌桶的周期。
	cleanupInterval = time.Minute
	// idleBucketTTL 是令牌桶的空闲存活时长，超过即回收。
	idleBucketTTL = 10 * time.Minute
	// maxBuckets 是令牌桶数量上限，避免大量来源 IP 把内存撑爆。
	maxBuckets = 10000
)

// RateLimit 按配置对请求做令牌桶限流。
//
//   - by=global：所有请求共享一个令牌桶；
//   - by=ip：每个客户端 IP 一个令牌桶（空闲桶会被回收，且总数有上限）。
//
// 被拒绝的请求返回 429，并通过 Retry-After 头告知建议等待的秒数。
// ctx 用于停止后台清理协程，通常传入进程的退出信号 context。
func RateLimit(ctx context.Context, cfg config.RateLimit, logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	if !cfg.Enabled || cfg.RPS <= 0 || cfg.Burst < 1 {
		// 配置不合法时宁可不限流，也不要让所有请求都被拒绝
		if cfg.Enabled {
			logger.Warn("限流配置不合法，已跳过限流",
				"rps", cfg.RPS, "burst", cfg.Burst)
		}
		return func(c *gin.Context) { c.Next() }
	}

	limiter := newTokenLimiter(cfg.RPS, cfg.Burst, maxBuckets)
	go limiter.run(ctx, cleanupInterval, idleBucketTTL)

	return func(c *gin.Context) {
		key := "global"
		if cfg.By == "ip" {
			key = c.ClientIP()
		}

		allowed, retryAfter := limiter.allow(key)
		if !allowed {
			seconds := int(math.Ceil(retryAfter.Seconds()))
			if seconds < 1 {
				seconds = 1
			}
			c.Header("Retry-After", strconv.Itoa(seconds))
			logger.Warn("请求被限流",
				"client_ip", c.ClientIP(),
				"method", c.Request.Method,
				"path", c.Request.URL.Path,
				"scope", cfg.By,
			)
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":       "too many requests",
				"scope":       cfg.By,
				"retry_after": seconds,
			})
			return
		}

		c.Next()
	}
}

// bucket 是某个 key 对应的令牌桶。
type bucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// tokenLimiter 管理一批令牌桶（每个 key 一个），并回收长期空闲的桶。
// now 可注入，便于测试回收逻辑时不必真的等待。
type tokenLimiter struct {
	mu         sync.Mutex
	limit      rate.Limit
	burst      int
	maxBuckets int
	buckets    map[string]*bucket
	now        func() time.Time
}

// newTokenLimiter 构造令牌桶管理器。
func newTokenLimiter(rps float64, burst, maxBuckets int) *tokenLimiter {
	return &tokenLimiter{
		limit:      rate.Limit(rps),
		burst:      burst,
		maxBuckets: maxBuckets,
		buckets:    make(map[string]*bucket),
		now:        time.Now,
	}
}

// allow 判断 key 当前是否放行；被拒绝时返回建议的等待时长。
func (l *tokenLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxBuckets {
			l.evictLocked(now)
		}
		b = &bucket{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[key] = b
	}
	b.lastSeen = now

	if b.limiter.Allow() {
		return true, 0
	}

	// 用 Reserve + Cancel 探测"再拿一个令牌要等多久"，不会真的消耗令牌
	reservation := b.limiter.Reserve()
	delay := reservation.DelayFrom(now)
	reservation.Cancel()
	return false, delay
}

// run 周期性回收空闲桶，直到 ctx 结束。
func (l *tokenLimiter) run(ctx context.Context, interval, idle time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.mu.Lock()
			l.cleanupLocked(l.now(), idle)
			l.mu.Unlock()
		}
	}
}

// cleanupLocked 回收空闲时长达到 idle 的桶，返回回收数量。
func (l *tokenLimiter) cleanupLocked(now time.Time, idle time.Duration) int {
	removed := 0
	for key, b := range l.buckets {
		if now.Sub(b.lastSeen) >= idle {
			delete(l.buckets, key)
			removed++
		}
	}
	return removed
}

// evictLocked 先回收空闲桶；若仍达上限，则淘汰最久未使用的一批。
// 调用方必须持有锁。
func (l *tokenLimiter) evictLocked(now time.Time) {
	if l.cleanupLocked(now, idleBucketTTL); len(l.buckets) < l.maxBuckets {
		return
	}

	keys := make([]string, 0, len(l.buckets))
	for key := range l.buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return l.buckets[keys[i]].lastSeen.Before(l.buckets[keys[j]].lastSeen)
	})

	// 腾出一个空位即可
	for _, key := range keys[:len(l.buckets)-l.maxBuckets+1] {
		delete(l.buckets, key)
	}
}

// size 返回当前桶数量（供测试断言内存占用）。
func (l *tokenLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
