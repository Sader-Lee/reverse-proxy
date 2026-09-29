// Package proxy 负责把客户端请求转发到后端实例。
//
// 阶段 1 基于 httputil.ReverseProxy 实现固定后端透传；多后端选择、健康检查与
// 超时重试分别在阶段 2、3、4 接入。
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"github.com/gin-gonic/gin"
)

// CtxKeyUpstream 是写入 gin.Context 的键，值为本次请求实际转发到的后端地址，
// 供访问日志等中间件读取。
const CtxKeyUpstream = "proxy.upstream"

// badGatewayMessage 是后端不可达时返回给客户端的错误标识。
const badGatewayMessage = "bad gateway"

// Handler 把收到的 HTTP 请求转发到后端实例。
type Handler struct {
	target *url.URL
	proxy  *httputil.ReverseProxy
	logger *slog.Logger
}

// New 构造一个把请求固定转发到 target 的处理器。
// target 需为 http/https 形式的完整地址，例如 http://127.0.0.1:9001。
func New(target string, logger *slog.Logger) (*Handler, error) {
	t, err := parseTarget(target)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	h := &Handler{target: t, logger: logger}

	rp := httputil.NewSingleHostReverseProxy(t)
	baseDirector := rp.Director
	rp.Director = func(req *http.Request) {
		scheme := "http"
		if req.TLS != nil {
			scheme = "https"
		}
		originalHost := req.Host

		// 改写 scheme/host/path（内部会保留原始 RawPath，避免 %2F 等编码丢失）
		baseDirector(req)

		// 让后端收到自己期望的 Host，而不是客户端请求代理时用的 Host
		req.Host = t.Host
		req.Header.Set("X-Forwarded-Proto", scheme)
		req.Header.Set("X-Forwarded-Host", originalHost)
		// X-Forwarded-For 由 ReverseProxy 自动追加，业务侧无需处理
	}

	// 周期性 flush：兼顾大响应吞吐与流式场景（event-stream 由标准库单独处理）
	rp.FlushInterval = 100 * time.Millisecond
	rp.Transport = newTransport()
	rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		h.handleUpstreamError(w, req, err)
	}

	h.proxy = rp
	return h, nil
}

// ServeHTTP 实现 http.Handler，便于在不依赖 Gin 的场景下直接使用。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.proxy.ServeHTTP(w, r)
}

// Handle 是 Gin 处理器版本：额外把转发目标写入上下文，供访问日志记录。
func (h *Handler) Handle(c *gin.Context) {
	c.Set(CtxKeyUpstream, h.target.String())
	h.proxy.ServeHTTP(c.Writer, c.Request)
}

// Target 返回当前固定转发的后端地址。
func (h *Handler) Target() string {
	return h.target.String()
}

// handleUpstreamError 在转发失败（连接失败、超时等）时回写 502。
func (h *Handler) handleUpstreamError(w http.ResponseWriter, req *http.Request, err error) {
	// 客户端主动取消请求不是后端故障，无需回写响应
	if errors.Is(err, context.Canceled) {
		h.logger.Debug("客户端取消请求",
			"method", req.Method,
			"path", req.URL.Path,
			"upstream", h.target.String(),
		)
		return
	}

	h.logger.Warn("转发失败",
		"method", req.Method,
		"path", req.URL.Path,
		"upstream", h.target.String(),
		"err", err.Error(),
	)

	body, marshalErr := json.Marshal(map[string]string{
		"error":    badGatewayMessage,
		"upstream": h.target.String(),
	})
	if marshalErr != nil {
		http.Error(w, badGatewayMessage, http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	if _, writeErr := w.Write(body); writeErr != nil {
		h.logger.Debug("回写错误响应失败", "err", writeErr.Error())
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

// parseTarget 解析并校验后端地址。
func parseTarget(target string) (*url.URL, error) {
	t, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("解析后端地址 %q 失败: %w", target, err)
	}
	if t.Scheme != "http" && t.Scheme != "https" {
		return nil, fmt.Errorf("后端地址 %q 的 scheme 必须是 http 或 https", target)
	}
	if t.Host == "" {
		return nil, fmt.Errorf("后端地址 %q 缺少主机名", target)
	}
	return t, nil
}
