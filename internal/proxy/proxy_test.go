package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
	"github.com/Sader-Lee/reverse-proxy/internal/balancer"
)

// discardLogger 返回一个丢弃全部输出的日志器，避免测试噪音。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// closeNotifierRecorder 在 httptest.ResponseRecorder 之上补上 CloseNotify。
//
// gin 的 responseWriter 声明实现了 http.CloseNotifier，并在 CloseNotify 内部对底层
// writer 做强制类型断言；而 httputil.ReverseProxy 在请求 context 没有取消信号时会探测
// 这个接口（httptest.NewRequest 构造的 context 恰好如此），直接传 ResponseRecorder
// 就会 panic。生产环境底层是 *http.response，不受影响。
type closeNotifierRecorder struct {
	*httptest.ResponseRecorder
	notify chan bool
}

func newRecorder() *closeNotifierRecorder {
	return &closeNotifierRecorder{
		ResponseRecorder: httptest.NewRecorder(),
		notify:           make(chan bool, 1),
	}
}

func (r *closeNotifierRecorder) CloseNotify() <-chan bool { return r.notify }

// testBackend 构造一个测试用后端实例。
func testBackend(t *testing.T, rawURL string, weight int) *backend.Backend {
	t.Helper()
	b, err := backend.New(rawURL, weight)
	if err != nil {
		t.Fatalf("构造后端 %s 失败: %v", rawURL, err)
	}
	return b
}

// newTestHandler 用给定后端构造轮询策略的处理器。
func newTestHandler(t *testing.T, backends ...*backend.Backend) *Handler {
	t.Helper()
	h, err := New(balancer.NewRoundRobin(backends), backend.NewRegistry(backends...), discardLogger())
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}
	return h
}

// newTestRouter 按与 main 一致的路由配置装配引擎：
// 开启 UseRawPath 并关闭路径反转义，确保测试覆盖真实的 URL 编码行为。
func newTestRouter(t *testing.T, handler *Handler) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.UseRawPath = true
	router.UnescapePathValues = false
	router.Any("/*path", handler.Handle)
	router.NoRoute(handler.Handle)
	return router
}

func TestNewRejectsNilDependencies(t *testing.T) {
	backends := []*backend.Backend{testBackend(t, "http://127.0.0.1:9001", 1)}

	if _, err := New(nil, backend.NewRegistry(backends...), discardLogger()); err == nil {
		t.Error("负载均衡器为 nil 时应报错")
	}
	if _, err := New(balancer.NewRoundRobin(backends), nil, discardLogger()); err == nil {
		t.Error("注册表为 nil 时应报错")
	}
}

func TestForwardsRequestAndSetsForwardedHeaders(t *testing.T) {
	var (
		gotMethod string
		gotPath   string
		gotQuery  string
		gotHost   string
		gotBody   string
		gotHeader http.Header
	)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotHost = r.Host
		gotBody = string(body)
		gotHeader = r.Header.Clone()

		w.Header().Set("X-Instance", "backend-a")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("from-backend"))
	}))
	defer upstream.Close()

	handler := newTestHandler(t, testBackend(t, upstream.URL, 1))
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/api/users?page=2", strings.NewReader(`{"name":"lee"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "1.2.3.4") // 客户端伪造的转发头应被丢弃
	w := newRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("状态码 = %d, 期望 %d", w.Code, http.StatusCreated)
	}
	if w.Body.String() != "from-backend" {
		t.Errorf("响应体 = %q, 期望 \"from-backend\"", w.Body.String())
	}
	if w.Header().Get("X-Instance") != "backend-a" {
		t.Errorf("后端响应头未透传，X-Instance = %q", w.Header().Get("X-Instance"))
	}
	if gotMethod != http.MethodPost {
		t.Errorf("后端收到方法 = %q, 期望 POST", gotMethod)
	}
	if gotPath != "/api/users" {
		t.Errorf("后端收到路径 = %q, 期望 \"/api/users\"", gotPath)
	}
	if gotQuery != "page=2" {
		t.Errorf("后端收到查询串 = %q, 期望 \"page=2\"", gotQuery)
	}
	if gotBody != `{"name":"lee"}` {
		t.Errorf("后端收到请求体 = %q, 期望 JSON 原样透传", gotBody)
	}
	if wantHost := strings.TrimPrefix(upstream.URL, "http://"); gotHost != wantHost {
		t.Errorf("后端收到 Host = %q, 期望被改写为 %q", gotHost, wantHost)
	}
	if got := gotHeader.Get("X-Forwarded-Proto"); got != "http" {
		t.Errorf("X-Forwarded-Proto = %q, 期望 \"http\"", got)
	}
	if got := gotHeader.Get("X-Forwarded-Host"); got != "proxy.example.com" {
		t.Errorf("X-Forwarded-Host = %q, 期望 \"proxy.example.com\"", got)
	}
	if got := gotHeader.Get("X-Forwarded-For"); !strings.Contains(got, "192.0.2.1") {
		t.Errorf("X-Forwarded-For = %q, 期望包含客户端 IP 192.0.2.1", got)
	}
	if got := gotHeader.Get("X-Forwarded-For"); strings.Contains(got, "1.2.3.4") {
		t.Errorf("X-Forwarded-For = %q, 客户端伪造的值应被丢弃", got)
	}
}

func TestPreservesEncodedPath(t *testing.T) {
	var gotRequestURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
	}))
	defer upstream.Close()

	handler := newTestHandler(t, testBackend(t, upstream.URL, 1))
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/files/a%2Fb/c.txt", nil)
	router.ServeHTTP(newRecorder(), req)

	if gotRequestURI != "/files/a%2Fb/c.txt" {
		t.Errorf("后端收到 RequestURI = %q, 期望 \"/files/a%%2Fb/c.txt\"（%%2F 不应被二次解码）", gotRequestURI)
	}
}

func TestPreservesBackendBasePath(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer upstream.Close()

	handler := newTestHandler(t, testBackend(t, upstream.URL+"/base", 1))
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/svc", nil)
	router.ServeHTTP(newRecorder(), req)

	if gotPath != "/base/svc" {
		t.Errorf("后端收到路径 = %q, 期望 \"/base/svc\"", gotPath)
	}
}

func TestDistributesRequestsRoundRobin(t *testing.T) {
	newUpstream := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Instance", name)
			_, _ = w.Write([]byte(name))
		}))
	}
	first, second := newUpstream("backend-a"), newUpstream("backend-b")
	defer first.Close()
	defer second.Close()

	handler := newTestHandler(t,
		testBackend(t, first.URL, 1),
		testBackend(t, second.URL, 1),
	)
	router := newTestRouter(t, handler)

	var got []string
	for i := 0; i < 4; i++ {
		w := newRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/ping", nil))
		got = append(got, w.Header().Get("X-Instance"))
	}

	want := []string{"backend-a", "backend-b", "backend-a", "backend-b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("轮询顺序 = %v, 期望 %v", got, want)
	}
}

func TestReturnsBadGatewayWhenUpstreamUnreachable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := upstream.URL
	upstream.Close() // 立刻关闭，制造必然连不上的后端

	instance := testBackend(t, target, 1)
	handler := newTestHandler(t, instance)
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/anything", nil)
	w := newRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", w.Code)
	}

	var payload map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v, 原始内容: %s", err, w.Body.String())
	}
	if payload["upstream"] != target {
		t.Errorf("响应中的 upstream = %q, 期望 %q", payload["upstream"], target)
	}
	if payload["error"] != "bad gateway" {
		t.Errorf("响应中的 error = %q, 期望 \"bad gateway\"", payload["error"])
	}

	stats := instance.Stats()
	if stats.Requests != 1 {
		t.Errorf("请求计数 = %d, 期望 1", stats.Requests)
	}
	if stats.Failures != 1 {
		t.Errorf("失败计数 = %d, 期望 1（转发失败应记账）", stats.Failures)
	}
}

func TestReturnsServiceUnavailableWhenNoHealthyBackend(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer upstream.Close()

	instance := testBackend(t, upstream.URL, 1)
	handler := newTestHandler(t, instance)
	router := newTestRouter(t, handler)

	instance.SetAlive(false)

	w := newRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/anything", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503", w.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v, 原始内容: %s", err, w.Body.String())
	}
	if payload["error"] != "no healthy backend" {
		t.Errorf("响应中的 error = %v, 期望 \"no healthy backend\"", payload["error"])
	}
	if payload["total_backends"] != float64(1) {
		t.Errorf("响应中的 total_backends = %v, 期望 1", payload["total_backends"])
	}
}

func TestHandleWritesUpstreamToContext(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	instance := testBackend(t, upstream.URL, 1)
	handler := newTestHandler(t, instance)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.UseRawPath = true
	router.Any("/*path", func(c *gin.Context) {
		handler.Handle(c)
		if got := c.GetString(CtxKeyUpstream); got != instance.String() {
			t.Errorf("上下文中的 upstream = %q, 期望 %q", got, instance.String())
		}
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/", nil)
	router.ServeHTTP(newRecorder(), req)
}

func TestServeHTTPWorksWithoutGin(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain"))
	}))
	defer upstream.Close()

	handler := newTestHandler(t, testBackend(t, upstream.URL, 1))

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/plain", nil).WithContext(context.Background())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK || w.Body.String() != "plain" {
		t.Errorf("直接使用 http.Handler 时转发失败: code=%d body=%q", w.Code, w.Body.String())
	}
}
