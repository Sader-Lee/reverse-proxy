// Package proxy 负责把客户端请求转发到后端实例。
//
// 转发目标由负载均衡器在每次请求时选出：后端不可达回写 502，
// 没有可用实例时回写 503。
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
	"github.com/Sader-Lee/reverse-proxy/internal/balancer"
)

// CtxKeyUpstream 是写入 gin.Context 的键，值为本次请求实际转发到的后端地址，
// 供访问日志等中间件读取。
const CtxKeyUpstream = "proxy.upstream"

const (
	badGatewayMessage = "bad gateway"
	noBackendMessage  = "no healthy backend"
)

// targetCtxKey 是在请求 context 中携带本次选中后端实例的键。
type targetCtxKey struct{}

// HealthReporter 接收转发失败通知，由健康检查器实现。
// 允许为 nil，表示不启用被动健康检查。
type HealthReporter interface {
	ReportFailure(b *backend.Backend)
}

// Handler 把收到的 HTTP 请求转发到负载均衡器选出的后端实例。
type Handler struct {
	balancer balancer.Balancer
	registry *backend.Registry
	reporter HealthReporter
	proxy    *httputil.ReverseProxy
	logger   *slog.Logger
}

// New 构造转发处理器。reporter 可为 nil。
func New(lb balancer.Balancer, registry *backend.Registry, reporter HealthReporter, logger *slog.Logger) (*Handler, error) {
	if lb == nil {
		return nil, errors.New("负载均衡器不能为空")
	}
	if registry == nil {
		return nil, errors.New("后端注册表不能为空")
	}
	if logger == nil {
		logger = slog.Default()
	}

	h := &Handler{balancer: lb, registry: registry, reporter: reporter, logger: logger}

	rp := &httputil.ReverseProxy{
		// 用 Rewrite（Go 1.20+）而不是 Director：它会先丢弃客户端伪造的
		// X-Forwarded-* 头，再由 SetXForwarded 写入可信值，避免头部欺骗。
		Rewrite: func(pr *httputil.ProxyRequest) {
			target := targetFrom(pr.In.Context())
			if target == nil {
				return // 理论上不可达：Handle 已保证选好实例
			}
			targetURL := target.URL()
			// SetURL 会改写 scheme/host/path（保留原始 RawPath 编码），并把 Host 头指向目标
			pr.SetURL(targetURL)
			pr.SetXForwarded()
		},
		Transport:     newTransport(),
		FlushInterval: 100 * time.Millisecond,
	}
	rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		h.handleUpstreamError(w, req, err)
	}

	h.proxy = rp
	return h, nil
}

// ServeHTTP 实现 http.Handler，便于在不依赖 Gin 的场景下使用。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	target := h.balancer.Next()
	if target == nil {
		h.writeNoHealthyBackend(w)
		return
	}

	target.RecordRequest()
	h.proxy.ServeHTTP(w, r.WithContext(withTarget(r.Context(), target)))
}

// Handle 是 Gin 处理器入口：先选后端，再交给 ReverseProxy 转发。
func (h *Handler) Handle(c *gin.Context) {
	target := h.balancer.Next()
	if target == nil {
		h.writeNoHealthyBackend(c.Writer)
		return
	}

	target.RecordRequest()
	c.Set(CtxKeyUpstream, target.String())
	h.proxy.ServeHTTP(c.Writer, c.Request.WithContext(withTarget(c.Request.Context(), target)))
}

// withTarget 把选中的后端实例放进请求 context。
func withTarget(ctx context.Context, target *backend.Backend) context.Context {
	return context.WithValue(ctx, targetCtxKey{}, target)
}

// targetFrom 取出本次请求选中的后端实例。
func targetFrom(ctx context.Context) *backend.Backend {
	target, _ := ctx.Value(targetCtxKey{}).(*backend.Backend)
	return target
}

// writeNoHealthyBackend 在没有任何存活实例时回写 503。
func (h *Handler) writeNoHealthyBackend(w http.ResponseWriter) {
	total := h.registry.Len()
	h.logger.Warn("没有可用的后端实例", "total", total)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"error":          noBackendMessage,
		"total_backends": total,
	}, h.logger)
}

// handleUpstreamError 在转发失败（连接失败、超时等）时累计失败次数并回写 502。
func (h *Handler) handleUpstreamError(w http.ResponseWriter, req *http.Request, err error) {
	target := targetFrom(req.Context())
	upstream := "unknown"
	if target != nil {
		upstream = target.String()
	}

	// 客户端主动断开导致请求 context 被取消：不是后端故障，
	// 既不回写响应，也不计入失败统计（否则会误抳后端）
	if errors.Is(err, context.Canceled) && req.Context().Err() != nil {
		h.logger.Debug("客户端取消请求",
			"method", req.Method,
			"path", req.URL.Path,
			"upstream", upstream,
		)
		return
	}

	if target != nil {
		target.RecordFailure() // 累计失败次数，供统计使用
		if h.reporter != nil {
			h.reporter.ReportFailure(target) // 通知健康检查器，参与连续失败计数
		}
	}

	h.logger.Warn("转发失败",
		"method", req.Method,
		"path", req.URL.Path,
		"upstream", upstream,
		"err", err.Error(),
	)

	writeJSON(w, http.StatusBadGateway, map[string]string{
		"error":    badGatewayMessage,
		"upstream": upstream,
	}, h.logger)
}

// writeJSON 回写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, payload any, logger *slog.Logger) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, http.StatusText(status), status)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		logger.Debug("回写响应失败", "err", err.Error())
	}
}

// newTransport 返回代理使用的传输层配置。
// 这里只设置连接层面的超时；单次请求的超时与重试由阶段 4 通过 context 控制。
func newTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
