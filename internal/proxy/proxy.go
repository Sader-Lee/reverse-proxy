// Package proxy 负责把客户端请求转发到后端实例。
//
// 每次请求由负载均衡器挑选实例；单次尝试超时回写 504，后端不可达回写 502，
// 没有可用实例回写 503。是否换实例重试由 Options 与请求方法共同决定：
// 响应在写回客户端之前才允许重试，因此失败尝试的响应会被整体丢弃。
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
	"github.com/Sader-Lee/reverse-proxy/internal/balancer"
)

// CtxKeyUpstream 是写入 gin.Context 的键，值为最后一次尝试的后端地址，
// 供访问日志等中间件读取。
const CtxKeyUpstream = "proxy.upstream"

const (
	badGatewayMessage     = "bad gateway"
	gatewayTimeoutMessage = "gateway timeout"
	noBackendMessage      = "no healthy backend"
)

// attemptCtxKey 是在请求 context 中携带本次尝试状态的键。
type attemptCtxKey struct{}

// HealthReporter 接收转发失败通知，由健康检查器实现。
// 允许为 nil，表示不启用被动健康检查。
type HealthReporter interface {
	ReportFailure(b *backend.Backend)
}

// Handler 把收到的 HTTP 请求转发到负载均衡器选出的后端实例。
type Handler struct {
	balancer    balancer.Balancer
	registry    *backend.Registry
	reporter    HealthReporter
	opts        Options
	retryStatus map[int]bool
	proxy       *httputil.ReverseProxy
	logger      *slog.Logger
}

// New 构造转发处理器。reporter 可为 nil，opts 为零值表示只尝试一次。
func New(lb balancer.Balancer, registry *backend.Registry, reporter HealthReporter, opts Options, logger *slog.Logger) (*Handler, error) {
	if lb == nil {
		return nil, errors.New("负载均衡器不能为空")
	}
	if registry == nil {
		return nil, errors.New("后端注册表不能为空")
	}
	if logger == nil {
		logger = slog.Default()
	}

	opts = opts.normalize()
	h := &Handler{
		balancer:    lb,
		registry:    registry,
		reporter:    reporter,
		opts:        opts,
		retryStatus: opts.retryStatusSet(),
		logger:      logger,
	}

	rp := &httputil.ReverseProxy{
		// 用 Rewrite（Go 1.20+）而不是 Director：它会先丢弃客户端伪造的
		// X-Forwarded-* 头，再由 SetXForwarded 写入可信值，避免头部欺骗。
		Rewrite: func(pr *httputil.ProxyRequest) {
			st := attemptFrom(pr.In.Context())
			if st == nil {
				return // 理论上不可达：forward 已保证状态存在
			}
			// SetURL 会改写 scheme/host/path（保留原始 RawPath 编码），并把 Host 头指向目标
			pr.SetURL(st.backend.URL())
			pr.SetXForwarded()
		},
		// 状态码触发的重试在这里判定：此时响应尚未写回客户端，
		// 返回哨兵错误即可让 ErrorHandler 接手，由 forward 决定换实例重试。
		ModifyResponse: func(res *http.Response) error {
			if res.Request == nil {
				return nil
			}
			st := attemptFrom(res.Request.Context())
			if st == nil || !st.shouldRetryStatus(res.StatusCode) {
				return nil
			}
			return retryableStatusError{status: res.StatusCode}
		},
		Transport:     newTransport(),
		FlushInterval: 100 * time.Millisecond,
	}
	// 只做判定与记账，不写响应；是否回写、能否重试统一由 forward 决定
	rp.ErrorHandler = func(_ http.ResponseWriter, req *http.Request, err error) {
		h.classifyAttemptError(req, err)
	}

	h.proxy = rp
	return h, nil
}

// ServeHTTP 实现 http.Handler，便于在不依赖 Gin 的场景下使用。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.forward(w, r)
}

// Handle 是 Gin 处理器入口：转发并记录最后一次尝试的后端实例。
func (h *Handler) Handle(c *gin.Context) {
	if last := h.forward(c.Writer, c.Request); last != nil {
		c.Set(CtxKeyUpstream, last.String())
	}
}

// forward 按重试策略把请求转发到后端，返回最后一次尝试的实例。
func (h *Handler) forward(w http.ResponseWriter, r *http.Request) *backend.Backend {
	maxAttempts := h.opts.MaxAttempts

	// 请求体需要缓冲才能重放；无法缓冲的请求（体过大、读取失败）只会尝试一次
	body, replayable := readReplayBody(r)

	exclude := make([]*backend.Backend, 0, maxAttempts)
	var (
		last      *backend.Backend
		lastError error
	)

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// 排除已尝试过的实例，避免重试打到同一个坏实例上
		target := h.balancer.Next(exclude...)
		if target == nil {
			break
		}
		exclude = append(exclude, target)
		last = target
		target.RecordRequest()

		st := &attemptState{
			attempt:       attempt,
			maxAttempts:   maxAttempts,
			backend:       target,
			retryStatus:   h.retryStatus,
			idempotent:    isIdempotent(r.Method),
			allowAnyRetry: h.opts.RetryNonIdempotent,
			replayable:    replayable,
		}

		ctx, cancel := h.attemptContext(r.Context())
		attemptReq := r.WithContext(withAttempt(ctx, st))
		if len(body) > 0 {
			// 每次尝试都需要独立的请求体读取器
			attemptReq.Body = io.NopCloser(bytes.NewReader(body))
		}
		h.proxy.ServeHTTP(w, attemptReq)
		cancel()

		if st.clientCanceled {
			return last // 客户端已断开，不回写任何响应
		}
		if st.failure() == nil {
			return last // 转发成功，或已按配置透传后端响应
		}
		if !st.retried {
			// 没有重试机会：记账并回写错误
			if st.transportErr != nil {
				h.reportFailure(target)
			}
			h.writeGatewayError(w, st.failure(), target)
			return last
		}

		if st.transportErr != nil {
			h.reportFailure(target)
		}
		lastError = st.failure()
		h.logger.Debug("本次尝试失败，换实例重试",
			"attempt", attempt,
			"max_attempts", maxAttempts,
			"method", r.Method,
			"path", r.URL.Path,
			"backend", target.String(),
			"reason", lastError.Error(),
		)
	}

	// 走出循环：已经没有可尝试的实例
	if lastError != nil {
		h.writeGatewayError(w, lastError, last)
		return last
	}
	h.writeNoHealthyBackend(w)
	return last
}

// attemptContext 为单次尝试生成 context（含单次超时）。
func (h *Handler) attemptContext(parent context.Context) (context.Context, context.CancelFunc) {
	if h.opts.PerTryTimeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, h.opts.PerTryTimeout)
}

// classifyAttemptError 判定一次尝试的失败类型，只记录不写响应。
func (h *Handler) classifyAttemptError(req *http.Request, err error) {
	st := attemptFrom(req.Context())
	if st == nil {
		return
	}

	// ModifyResponse 判定"状态码需要重试"时会走到这里
	var statusErr retryableStatusError
	if errors.As(err, &statusErr) {
		st.retried = true
		st.statusToRetry = statusErr.status
		return
	}

	// 客户端主动断开：不是后端故障，也不回写响应
	if errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		st.clientCanceled = true
		h.logger.Debug("客户端取消请求",
			"method", req.Method,
			"path", req.URL.Path,
			"upstream", st.backend.String(),
		)
		return
	}

	st.transportErr = err
	if st.canRetryTransport(err) {
		st.retried = true
	}
}

// reportFailure 记录一次后端故障：累计统计并通知健康检查器。
func (h *Handler) reportFailure(target *backend.Backend) {
	target.RecordFailure()
	if h.reporter != nil {
		h.reporter.ReportFailure(target)
	}
}

// withAttempt 把本次尝试的状态放进请求 context。
func withAttempt(ctx context.Context, st *attemptState) context.Context {
	return context.WithValue(ctx, attemptCtxKey{}, st)
}

// attemptFrom 取出本次尝试的状态。
func attemptFrom(ctx context.Context) *attemptState {
	st, _ := ctx.Value(attemptCtxKey{}).(*attemptState)
	return st
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

// writeGatewayError 回写上游错误：单次尝试超时为 504，其余为 502。
func (h *Handler) writeGatewayError(w http.ResponseWriter, err error, target *backend.Backend) {
	upstream := "unknown"
	if target != nil {
		upstream = target.String()
	}

	status := http.StatusBadGateway
	message := badGatewayMessage
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
		message = gatewayTimeoutMessage
	}

	payload := map[string]any{
		"error":    message,
		"upstream": upstream,
	}
	var statusErr retryableStatusError
	if errors.As(err, &statusErr) {
		payload["last_status"] = statusErr.status
	}

	h.logger.Warn("转发失败",
		"upstream", upstream,
		"status", status,
		"err", err.Error(),
	)

	writeJSON(w, status, payload, h.logger)
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
// 单次请求的超时由 forward 通过 context 控制，这里只设置连接层面的超时。
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
