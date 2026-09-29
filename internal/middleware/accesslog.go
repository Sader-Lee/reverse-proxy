// Package middleware 提供代理服务使用的 Gin 中间件。
package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sader-Lee/reverse-proxy/internal/proxy"
)

// AccessLog 记录每次请求的访问日志：
// 方法、路径、查询串、状态码、响应字节数、耗时、客户端 IP 与转发目标。
// 4xx/5xx 以 WARN 级别输出，便于直接过滤异常流量。
func AccessLog(logger *slog.Logger) gin.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}

	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		elapsed := time.Since(start)

		status := c.Writer.Status()
		attrs := []slog.Attr{
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", status),
			slog.Int("bytes", c.Writer.Size()),
			slog.Float64("latency_ms", float64(elapsed.Microseconds())/1000),
			slog.String("client_ip", c.ClientIP()),
		}
		if query := c.Request.URL.RawQuery; query != "" {
			attrs = append(attrs, slog.String("query", query))
		}
		// 路径被客户端编码过时（如 %2F），额外记录原始形式，便于排查转发错位
		if escaped := c.Request.URL.EscapedPath(); escaped != c.Request.URL.Path {
			attrs = append(attrs, slog.String("escaped_path", escaped))
		}
		if upstream := c.GetString(proxy.CtxKeyUpstream); upstream != "" {
			attrs = append(attrs, slog.String("upstream", upstream))
		}
		if agent := c.Request.UserAgent(); agent != "" {
			attrs = append(attrs, slog.String("user_agent", agent))
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, slog.String("error", c.Errors.String()))
		}

		level := slog.LevelInfo
		if status >= http.StatusBadRequest {
			level = slog.LevelWarn
		}
		logger.LogAttrs(c.Request.Context(), level, "access", attrs...)
	}
}
