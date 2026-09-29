package middleware

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sader-Lee/reverse-proxy/internal/config"
)

// silentLogger 返回丢弃输出的日志器，避免测试噪音。
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// rateLimitedRouter 装配一个只挂限流中间件的引擎。
// 与生产一致地关闭受信代理，因此 ClientIP 始终取真实 TCP 来源。
func rateLimitedRouter(t *testing.T, cfg config.RateLimit) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	if err := router.SetTrustedProxies(nil); err != nil {
		t.Fatalf("关闭受信代理失败: %v", err)
	}
	router.Use(RateLimit(context.Background(), cfg, silentLogger()))
	router.GET("/ping", func(c *gin.Context) {
		c.String(http.StatusOK, "pong")
	})
	return router
}

// requestFrom 从指定来源 IP 发起请求，可选地带上伪造的 X-Forwarded-For。
func requestFrom(router *gin.Engine, clientAddr, forwardedFor string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/ping", nil)
	req.RemoteAddr = clientAddr
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestRateLimitAllowsBurstThenRejects(t *testing.T) {
	// rps=1、burst=2：前两个请求放行，第三个被拒
	router := rateLimitedRouter(t, config.RateLimit{Enabled: true, RPS: 1, Burst: 2, By: "global"})

	for i := 1; i <= 2; i++ {
		if w := requestFrom(router, "192.0.2.1:1000", ""); w.Code != http.StatusOK {
			t.Fatalf("第 %d 个请求状态码 = %d, 期望 200（burst 内应放行）", i, w.Code)
		}
	}

	w := requestFrom(router, "192.0.2.1:1000", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("超出 burst 后状态码 = %d, 期望 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Errorf("Retry-After = %q, 期望给出建议等待秒数", got)
	}

	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v, 原始内容: %s", err, w.Body.String())
	}
	if payload["error"] != "too many requests" {
		t.Errorf("响应中的 error = %v, 期望 too many requests", payload["error"])
	}
	if payload["scope"] != "global" {
		t.Errorf("响应中的 scope = %v, 期望 global", payload["scope"])
	}
}

func TestRateLimitDisabledPassesThrough(t *testing.T) {
	// 即使速率低到几乎不放行，只要没启用就不应拦截
	router := rateLimitedRouter(t, config.RateLimit{Enabled: false, RPS: 0.0001, Burst: 1, By: "ip"})

	for i := 0; i < 5; i++ {
		if w := requestFrom(router, "192.0.2.1:1000", ""); w.Code != http.StatusOK {
			t.Fatalf("未启用限流时第 %d 个请求状态码 = %d, 期望 200", i+1, w.Code)
		}
	}
}

func TestRateLimitSkipsInvalidConfig(t *testing.T) {
	// 启用但参数非法：宁可不限流，也不能把所有请求都拒掉
	router := rateLimitedRouter(t, config.RateLimit{Enabled: true, RPS: 0, Burst: 0, By: "global"})

	for i := 0; i < 3; i++ {
		if w := requestFrom(router, "192.0.2.1:1000", ""); w.Code != http.StatusOK {
			t.Fatalf("参数非法时第 %d 个请求状态码 = %d, 期望 200（跳过限流）", i+1, w.Code)
		}
	}
}

func TestRateLimitPerIPIsolatesClients(t *testing.T) {
	// 每个 IP 独立桶：一个 IP 用尽不应影响另一个
	router := rateLimitedRouter(t, config.RateLimit{Enabled: true, RPS: 0.0001, Burst: 1, By: "ip"})

	if w := requestFrom(router, "192.0.2.1:1000", ""); w.Code != http.StatusOK {
		t.Fatalf("客户端 A 首个请求 = %d, 期望 200", w.Code)
	}
	if w := requestFrom(router, "192.0.2.1:1001", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("客户端 A 第二个请求 = %d, 期望 429（同一 IP 共用桶）", w.Code)
	}
	if w := requestFrom(router, "198.51.100.9:2000", ""); w.Code != http.StatusOK {
		t.Fatalf("客户端 B 首个请求 = %d, 期望 200（不同 IP 桶独立）", w.Code)
	}
}

func TestRateLimitIgnoresSpoofedForwardedFor(t *testing.T) {
	// 关闭受信代理后，伪造 X-Forwarded-For 不能换到新的桶，否则限流形同虚设
	router := rateLimitedRouter(t, config.RateLimit{Enabled: true, RPS: 0.0001, Burst: 1, By: "ip"})

	if w := requestFrom(router, "192.0.2.1:1000", "1.1.1.1"); w.Code != http.StatusOK {
		t.Fatalf("首个请求 = %d, 期望 200", w.Code)
	}
	if w := requestFrom(router, "192.0.2.1:1001", "2.2.2.2"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("换一个伪造 XFF 后 = %d, 期望 429（仍按真实来源 IP 限流）", w.Code)
	}
}

func TestRateLimitRefillsOverTime(t *testing.T) {
	// rps=100 表示每 10ms 补一个令牌
	router := rateLimitedRouter(t, config.RateLimit{Enabled: true, RPS: 100, Burst: 1, By: "global"})

	if w := requestFrom(router, "192.0.2.1:1000", ""); w.Code != http.StatusOK {
		t.Fatalf("首个请求 = %d, 期望 200", w.Code)
	}
	if w := requestFrom(router, "192.0.2.1:1000", ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("紧接着的请求 = %d, 期望 429", w.Code)
	}

	time.Sleep(60 * time.Millisecond)
	if w := requestFrom(router, "192.0.2.1:1000", ""); w.Code != http.StatusOK {
		t.Fatalf("等待补充令牌后 = %d, 期望 200", w.Code)
	}
}

func TestTokenLimiterCleanupEvictsIdleBuckets(t *testing.T) {
	limiter := newTokenLimiter(10, 5, 100)

	now := time.Now()
	limiter.now = func() time.Time { return now }

	limiter.allow("a")
	limiter.allow("b")
	if got := limiter.size(); got != 2 {
		t.Fatalf("桶数量 = %d, 期望 2", got)
	}

	// 未到空闲阈值：不回收
	if removed := limiter.cleanupLocked(now.Add(time.Minute), 10*time.Minute); removed != 0 {
		t.Errorf("回收数量 = %d, 期望 0", removed)
	}
	if got := limiter.size(); got != 2 {
		t.Errorf("桶数量 = %d, 期望 2", got)
	}

	// 超过空闲阈值：全部回收
	if removed := limiter.cleanupLocked(now.Add(11*time.Minute), 10*time.Minute); removed != 2 {
		t.Errorf("回收数量 = %d, 期望 2", removed)
	}
	if got := limiter.size(); got != 0 {
		t.Errorf("回收后桶数量 = %d, 期望 0", got)
	}
}

func TestTokenLimiterIsBoundedByMaxBuckets(t *testing.T) {
	const maxBuckets = 8
	limiter := newTokenLimiter(10, 5, maxBuckets)

	for i := 0; i < 100; i++ {
		limiter.allow("client-" + string(rune('a'+i%26)) + string(rune('0'+i/26)))
		if size := limiter.size(); size > maxBuckets {
			t.Fatalf("第 %d 个来源后桶数量 = %d, 超过上限 %d", i+1, size, maxBuckets)
		}
	}
}

func TestTokenLimiterJanitorEvictsIdleBuckets(t *testing.T) {
	limiter := newTokenLimiter(10, 5, 100)
	limiter.allow("client")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go limiter.run(ctx, 5*time.Millisecond, time.Millisecond)

	deadline := time.After(2 * time.Second)
	for limiter.size() > 0 {
		select {
		case <-deadline:
			t.Fatal("后台清理协程未回收空闲桶")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestTokenLimiterEvictionPrefersIdleBuckets(t *testing.T) {
	limiter := newTokenLimiter(10, 5, 3)

	base := time.Now()
	limiter.now = func() time.Time { return base }
	limiter.allow("a")
	limiter.allow("b")
	limiter.allow("c")

	// 三个桶都已空闲超过 TTL，此时新来源到来：
	// 应通过回收空闲桶腾出位置，而不是淘汰最近使用的桶
	limiter.now = func() time.Time { return base.Add(idleBucketTTL + time.Minute) }
	limiter.allow("d")

	if got := limiter.size(); got != 1 {
		t.Errorf("桶数量 = %d, 期望 1（三个空闲桶被回收，只剩新桶）", got)
	}
}

func TestTokenLimiterEvictsLeastRecentlyUsed(t *testing.T) {
	limiter := newTokenLimiter(10, 5, 3)

	// 三个桶的最后使用时刻必须互不相同：
	// 若存在并列，map 迭代顺序随机 + sort.Slice 不稳定会让"淘汰谁"变得不确定
	base := time.Now()
	limiter.now = func() time.Time { return base }
	limiter.allow("oldest")

	limiter.now = func() time.Time { return base.Add(30 * time.Second) }
	limiter.allow("middle")

	limiter.now = func() time.Time { return base.Add(time.Minute) }
	limiter.allow("newest")

	// 触发一次淘汰：最久未使用的 oldest 应被移除
	limiter.now = func() time.Time { return base.Add(2 * time.Minute) }
	limiter.allow("another")

	limiter.mu.Lock()
	_, hasOldest := limiter.buckets["oldest"]
	size := len(limiter.buckets)
	limiter.mu.Unlock()

	if hasOldest {
		t.Error("最久未使用的桶应被淘汰")
	}
	if size > 3 {
		t.Errorf("桶数量 = %d, 期望不超过 3", size)
	}
}
