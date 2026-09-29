package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// namedUpstream 起一个带实例名的后端；status 非 200 时固定返回该状态码。
func namedUpstream(t *testing.T, name string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Instance", name)
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(name + ":" + strconv.Itoa(status)))
			return
		}
		_, _ = w.Write([]byte(name))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadAddress 返回一个必然连接失败的后端地址。
func deadAddress(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()
	return addr
}

// newTestHandlerWith 用给定后端构造处理器（不接健康检查）。
func newTestHandlerWith(t *testing.T, reporter HealthReporter, opts Options, backends ...*backend.Backend) *Handler {
	t.Helper()
	h, err := New(balancer.NewRoundRobin(backends), backend.NewRegistry(backends...), reporter, opts, discardLogger())
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}
	return h
}

// newTestHandler 构造单次尝试（不重试）的处理器。
func newTestHandler(t *testing.T, backends ...*backend.Backend) *Handler {
	t.Helper()
	return newTestHandlerWith(t, nil, Options{}, backends...)
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

// serve 通过 gin 路由发起一次请求。
func serve(router *gin.Engine, req *http.Request) *closeNotifierRecorder {
	w := newRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestNewRejectsNilDependencies(t *testing.T) {
	backends := []*backend.Backend{testBackend(t, "http://127.0.0.1:9001", 1)}

	if _, err := New(nil, backend.NewRegistry(backends...), nil, Options{}, discardLogger()); err == nil {
		t.Error("负载均衡器为 nil 时应报错")
	}
	if _, err := New(balancer.NewRoundRobin(backends), nil, nil, Options{}, discardLogger()); err == nil {
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
	w := serve(router, req)

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
	serve(router, req)

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
	serve(router, req)

	if gotPath != "/base/svc" {
		t.Errorf("后端收到路径 = %q, 期望 \"/base/svc\"", gotPath)
	}
}

func TestDistributesRequestsRoundRobin(t *testing.T) {
	first, second := namedUpstream(t, "backend-a", http.StatusOK), namedUpstream(t, "backend-b", http.StatusOK)

	handler := newTestHandler(t,
		testBackend(t, first.URL, 1),
		testBackend(t, second.URL, 1),
	)
	router := newTestRouter(t, handler)

	var got []string
	for i := 0; i < 4; i++ {
		w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/ping", nil))
		got = append(got, w.Header().Get("X-Instance"))
	}

	want := []string{"backend-a", "backend-b", "backend-a", "backend-b"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("轮询顺序 = %v, 期望 %v", got, want)
	}
}

func TestReturnsBadGatewayWhenUpstreamUnreachable(t *testing.T) {
	target := deadAddress(t)
	instance := testBackend(t, target, 1)
	handler := newTestHandler(t, instance)
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/anything", nil)
	w := serve(router, req)

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
	upstream := namedUpstream(t, "backend-a", http.StatusOK)

	instance := testBackend(t, upstream.URL, 1)
	handler := newTestHandler(t, instance)
	router := newTestRouter(t, handler)

	instance.SetAlive(false)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/anything", nil))

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
	upstream := namedUpstream(t, "backend-a", http.StatusOK)

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

// fakeReporter 记录收到的转发失败通知。
type fakeReporter struct {
	mu       sync.Mutex
	failures []*backend.Backend
}

func (f *fakeReporter) ReportFailure(b *backend.Backend) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, b)
}

func (f *fakeReporter) reported() []*backend.Backend {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*backend.Backend(nil), f.failures...)
}

func TestReportsForwardFailureToReporter(t *testing.T) {
	instance := testBackend(t, deadAddress(t), 1)
	reporter := &fakeReporter{}
	handler := newTestHandlerWith(t, reporter, Options{}, instance)

	router := newTestRouter(t, handler)
	serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/x", nil))

	reported := reporter.reported()
	if len(reported) != 1 || reported[0] != instance {
		t.Errorf("上报的失败实例 = %v, 期望恰好一次且为 %s", reported, instance.String())
	}
}

func TestDoesNotCountClientCancellationAsFailure(t *testing.T) {
	upstream := namedUpstream(t, "backend-a", http.StatusOK)
	instance := testBackend(t, upstream.URL, 1)
	reporter := &fakeReporter{}
	handler := newTestHandlerWith(t, reporter, Options{}, instance)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/x", nil).WithContext(ctx)

	handler.ServeHTTP(httptest.NewRecorder(), req)

	if got := reporter.reported(); len(got) != 0 {
		t.Errorf("客户端取消不应上报后端失败，实际上报 %d 次", len(got))
	}
	if stats := instance.Stats(); stats.Failures != 0 {
		t.Errorf("客户端取消不应计入失败统计，实际 %d 次", stats.Failures)
	}
}

func TestRetriesOnConnectionFailureWithAnotherBackend(t *testing.T) {
	alive := namedUpstream(t, "backend-b", http.StatusOK)
	dead := testBackend(t, deadAddress(t), 1)
	live := testBackend(t, alive.URL, 1)

	handler := newTestHandlerWith(t, nil, Options{MaxAttempts: 2}, dead, live)
	router := newTestRouter(t, handler)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/retry", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（应重试到存活实例）", w.Code)
	}
	if got := w.Header().Get("X-Instance"); got != "backend-b" {
		t.Errorf("响应来自 %q, 期望 backend-b", got)
	}
	if stats := dead.Stats(); stats.Requests != 1 || stats.Failures != 1 {
		t.Errorf("失败实例统计 = %+v, 期望 Requests=1 Failures=1", stats)
	}
	if stats := live.Stats(); stats.Requests != 1 || stats.Failures != 0 {
		t.Errorf("成功实例统计 = %+v, 期望 Requests=1 Failures=0", stats)
	}
}

func TestRetryStopsWhenNoMoreBackends(t *testing.T) {
	// 三个实例全部不可达，只配两个尝试次数
	instances := []*backend.Backend{
		testBackend(t, deadAddress(t), 1),
		testBackend(t, deadAddress(t), 1),
		testBackend(t, deadAddress(t), 1),
	}
	handler := newTestHandlerWith(t, nil, Options{MaxAttempts: 2}, instances...)
	router := newTestRouter(t, handler)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/x", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", w.Code)
	}
	total := int64(0)
	for _, b := range instances {
		total += b.Stats().Requests
	}
	if total != 2 {
		t.Errorf("总尝试次数 = %d, 期望 2", total)
	}
}

func TestRetryExhaustedReturnsBadGateway(t *testing.T) {
	instances := []*backend.Backend{
		testBackend(t, deadAddress(t), 1),
		testBackend(t, deadAddress(t), 1),
		testBackend(t, deadAddress(t), 1),
	}
	reporter := &fakeReporter{}
	handler := newTestHandlerWith(t, reporter, Options{MaxAttempts: 3}, instances...)
	router := newTestRouter(t, handler)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/x", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", w.Code)
	}
	if got := len(reporter.reported()); got != 3 {
		t.Errorf("上报失败次数 = %d, 期望 3（每次尝试都记账）", got)
	}
	// 每次尝试都应换一个实例，不能重复打同一个坏实例
	seen := map[*backend.Backend]bool{}
	for _, b := range reporter.reported() {
		if seen[b] {
			t.Errorf("实例 %s 被重复尝试", b.String())
		}
		seen[b] = true
	}
}

func TestRetriesOnRetryableStatus(t *testing.T) {
	bad := namedUpstream(t, "backend-a", http.StatusServiceUnavailable)
	good := namedUpstream(t, "backend-b", http.StatusOK)

	handler := newTestHandlerWith(t, nil, Options{
		MaxAttempts:   2,
		RetryOnStatus: []int{http.StatusServiceUnavailable},
	}, testBackend(t, bad.URL, 1), testBackend(t, good.URL, 1))
	router := newTestRouter(t, handler)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/x", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（503 应触发换实例重试）", w.Code)
	}
	if got := w.Header().Get("X-Instance"); got != "backend-b" {
		t.Errorf("响应来自 %q, 期望 backend-b", got)
	}
}

func TestRetriesOnClientErrorStatusWhenConfigured(t *testing.T) {
	bad := namedUpstream(t, "backend-a", http.StatusNotFound)
	good := namedUpstream(t, "backend-b", http.StatusOK)

	handler := newTestHandlerWith(t, nil, Options{
		MaxAttempts:   2,
		RetryOnStatus: []int{http.StatusNotFound},
	}, testBackend(t, bad.URL, 1), testBackend(t, good.URL, 1))
	router := newTestRouter(t, handler)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/x", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（配置了 404 重试后应换实例）", w.Code)
	}
	if got := w.Header().Get("X-Instance"); got != "backend-b" {
		t.Errorf("响应来自 %q, 期望 backend-b", got)
	}
}

func TestPassesThroughRetryableStatusWhenNoAttemptsLeft(t *testing.T) {
	bad := namedUpstream(t, "backend-a", http.StatusServiceUnavailable)

	handler := newTestHandlerWith(t, nil, Options{
		MaxAttempts:   1,
		RetryOnStatus: []int{http.StatusServiceUnavailable},
	}, testBackend(t, bad.URL, 1))
	router := newTestRouter(t, handler)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/x", nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503（没有剩余尝试次数时应原样透传后端响应）", w.Code)
	}
	if w.Body.String() != "backend-a:503" {
		t.Errorf("响应体 = %q, 期望透传后端响应体", w.Body.String())
	}
}

func TestDoesNotRetryNonIdempotentOnRetryableStatus(t *testing.T) {
	bad := namedUpstream(t, "backend-a", http.StatusServiceUnavailable)
	good := namedUpstream(t, "backend-b", http.StatusOK)

	goodInstance := testBackend(t, good.URL, 1)
	handler := newTestHandlerWith(t, nil, Options{
		MaxAttempts:   2,
		RetryOnStatus: []int{http.StatusServiceUnavailable},
	}, testBackend(t, bad.URL, 1), goodInstance)
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/order", strings.NewReader(`{"id":1}`))
	w := serve(router, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d, 期望 503（非幂等方法默认不按状态码重试）", w.Code)
	}
	if stats := goodInstance.Stats(); stats.Requests != 0 {
		t.Errorf("健康实例被请求 %d 次, 期望 0（不应重试）", stats.Requests)
	}
}

func TestRetriesNonIdempotentOnDialError(t *testing.T) {
	good := namedUpstream(t, "backend-b", http.StatusOK)

	handler := newTestHandlerWith(t, nil, Options{MaxAttempts: 2},
		testBackend(t, deadAddress(t), 1), testBackend(t, good.URL, 1))
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/order", strings.NewReader(`{"id":1}`))
	w := serve(router, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（连接都没建立起来时非幂等方法可以安全重试）", w.Code)
	}
	if got := w.Header().Get("X-Instance"); got != "backend-b" {
		t.Errorf("响应来自 %q, 期望 backend-b", got)
	}
}

func TestRetriesNonIdempotentWhenEnabled(t *testing.T) {
	bad := namedUpstream(t, "backend-a", http.StatusServiceUnavailable)
	good := namedUpstream(t, "backend-b", http.StatusOK)

	handler := newTestHandlerWith(t, nil, Options{
		MaxAttempts:        2,
		RetryOnStatus:      []int{http.StatusServiceUnavailable},
		RetryNonIdempotent: true,
	}, testBackend(t, bad.URL, 1), testBackend(t, good.URL, 1))
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/order", strings.NewReader(`{"id":1}`))
	w := serve(router, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（开启后非幂等方法也应重试）", w.Code)
	}
	if got := w.Header().Get("X-Instance"); got != "backend-b" {
		t.Errorf("响应来自 %q, 期望 backend-b", got)
	}
}

func TestRetryStopsWhenAllBackendsExcluded(t *testing.T) {
	instances := []*backend.Backend{
		testBackend(t, deadAddress(t), 1),
		testBackend(t, deadAddress(t), 1),
	}
	// max_attempts 大于实例数量：应在用完所有实例后停止并回写错误，
	// 而不是反复尝试同一个坏实例
	handler := newTestHandlerWith(t, nil, Options{MaxAttempts: 5}, instances...)
	router := newTestRouter(t, handler)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/x", nil))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", w.Code)
	}
	for _, b := range instances {
		if stats := b.Stats(); stats.Requests != 1 {
			t.Errorf("实例 %s 被尝试 %d 次, 期望 1（不应重复尝试同一实例）", b.String(), stats.Requests)
		}
	}
}

func TestRetryReplaysRequestBody(t *testing.T) {
	var (
		mu       sync.Mutex
		recorded = map[string][]string{}
	)

	upstream := func(name string, status int) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			recorded[name] = append(recorded[name], string(body))
			mu.Unlock()

			w.Header().Set("X-Instance", name)
			w.WriteHeader(status)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	bad := upstream("backend-a", http.StatusServiceUnavailable)
	good := upstream("backend-b", http.StatusOK)

	handler := newTestHandlerWith(t, nil, Options{
		MaxAttempts:        2,
		RetryOnStatus:      []int{http.StatusServiceUnavailable},
		RetryNonIdempotent: true,
	}, testBackend(t, bad.URL, 1), testBackend(t, good.URL, 1))
	router := newTestRouter(t, handler)

	payload := `{"id":1}`
	req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/order", strings.NewReader(payload))
	w := serve(router, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", w.Code)
	}
	if got := w.Header().Get("X-Instance"); got != "backend-b" {
		t.Errorf("响应来自 %q, 期望 backend-b", got)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"backend-a", "backend-b"} {
		bodies := recorded[name]
		if len(bodies) != 1 {
			t.Fatalf("%s 收到 %d 次请求, 期望 1", name, len(bodies))
		}
		if bodies[0] != payload {
			t.Errorf("%s 收到的请求体 = %q, 期望 %q（重试必须重放完整请求体）", name, bodies[0], payload)
		}
	}
}

func TestPerTryTimeoutReturnsGatewayTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("too late"))
	}))
	t.Cleanup(slow.Close)

	handler := newTestHandlerWith(t, nil, Options{
		MaxAttempts:   1,
		PerTryTimeout: 30 * time.Millisecond,
	}, testBackend(t, slow.URL, 1))
	router := newTestRouter(t, handler)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/slow", nil))

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("状态码 = %d, 期望 504", w.Code)
	}

	var payload map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v", err)
	}
	if payload["error"] != "gateway timeout" {
		t.Errorf("响应中的 error = %v, 期望 \"gateway timeout\"", payload["error"])
	}
}

func TestPerTryTimeoutFallsBackToOtherBackend(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("slow"))
	}))
	t.Cleanup(slow.Close)
	fast := namedUpstream(t, "backend-b", http.StatusOK)

	slowInstance := testBackend(t, slow.URL, 1)
	handler := newTestHandlerWith(t, nil, Options{
		MaxAttempts:   2,
		PerTryTimeout: 50 * time.Millisecond,
	}, slowInstance, testBackend(t, fast.URL, 1))
	router := newTestRouter(t, handler)

	w := serve(router, httptest.NewRequest(http.MethodGet, "http://proxy.example.com/slow", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200（超时后应换实例重试）", w.Code)
	}
	if got := w.Header().Get("X-Instance"); got != "backend-b" {
		t.Errorf("响应来自 %q, 期望 backend-b", got)
	}
	if stats := slowInstance.Stats(); stats.Failures != 1 {
		t.Errorf("超时实例失败计数 = %d, 期望 1", stats.Failures)
	}
}

func TestLargeBodyIsStreamedIntact(t *testing.T) {
	var gotLen int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotLen = len(body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	handler := newTestHandlerWith(t, nil, Options{MaxAttempts: 3}, testBackend(t, upstream.URL, 1))
	router := newTestRouter(t, handler)

	// 超过 maxReplayBodyBytes 的请求体不会被缓冲，但仍必须完整转发
	large := strings.Repeat("x", maxReplayBodyBytes+1024)
	req := httptest.NewRequest(http.MethodPut, "http://proxy.example.com/upload", strings.NewReader(large))
	w := serve(router, req)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 = %d, 期望 200", w.Code)
	}
	if gotLen != len(large) {
		t.Errorf("后端收到请求体长度 = %d, 期望 %d（超限请求体应原样流式转发）", gotLen, len(large))
	}
}

func TestDoesNotRetryNonIdempotentOnTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	t.Cleanup(slow.Close)

	slowInstance := testBackend(t, slow.URL, 1)
	fastInstance := testBackend(t, namedUpstream(t, "backend-b", http.StatusOK).URL, 1)

	handler := newTestHandlerWith(t, nil, Options{
		MaxAttempts:   3,
		PerTryTimeout: 30 * time.Millisecond,
	}, slowInstance, fastInstance)
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/order", strings.NewReader(`{"id":1}`))
	w := serve(router, req)

	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("状态码 = %d, 期望 504", w.Code)
	}
	if stats := slowInstance.Stats(); stats.Requests != 1 {
		t.Errorf("超时实例被尝试 %d 次, 期望 1（非幂等方法超时不应重试）", stats.Requests)
	}
	if stats := fastInstance.Stats(); stats.Requests != 0 {
		t.Errorf("健康实例被请求 %d 次, 期望 0（不应换实例重试）", stats.Requests)
	}
}

func TestLargeBodyIsNotRetried(t *testing.T) {
	fastInstance := testBackend(t, namedUpstream(t, "backend-b", http.StatusOK).URL, 1)

	handler := newTestHandlerWith(t, nil, Options{MaxAttempts: 3},
		testBackend(t, deadAddress(t), 1), fastInstance)
	router := newTestRouter(t, handler)

	// 超过 maxReplayBodyBytes 的请求体无法缓冲重放，
	// 因此即使还有健康实例也不会重试
	large := strings.Repeat("x", maxReplayBodyBytes+1024)
	req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/upload", strings.NewReader(large))
	w := serve(router, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("状态码 = %d, 期望 502", w.Code)
	}
	if stats := fastInstance.Stats(); stats.Requests != 0 {
		t.Errorf("健康实例被请求 %d 次, 期望 0（请求体不可重放时不重试）", stats.Requests)
	}
}

func TestReadReplayBody(t *testing.T) {
	t.Run("无请求体", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/x", nil)
		body, replayable := readReplayBody(req)
		if !replayable || len(body) != 0 {
			t.Errorf("无请求体时应返回 (空, true)，实际 (%q, %v)", body, replayable)
		}
	})

	t.Run("小请求体可重放", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/x", strings.NewReader("payload"))
		body, replayable := readReplayBody(req)
		if !replayable || string(body) != "payload" {
			t.Errorf("应返回 (payload, true)，实际 (%q, %v)", body, replayable)
		}
	})

	t.Run("超大请求体不可重放但流仍完整", func(t *testing.T) {
		payload := strings.Repeat("y", maxReplayBodyBytes+1)
		req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/x", strings.NewReader(payload))

		body, replayable := readReplayBody(req)
		if replayable || body != nil {
			t.Errorf("超限时应返回 (nil, false)，实际 (%v, %v)", body, replayable)
		}

		// r.Body 已被拼回完整流，转发仍能读到全部内容，关闭时也不应报错
		rest, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("读取拼回的请求体失败: %v", err)
		}
		if string(rest) != payload {
			t.Errorf("拼回的请求体长度 = %d, 期望 %d", len(rest), len(payload))
		}
		if err := req.Body.Close(); err != nil {
			t.Errorf("关闭拼回的请求体失败: %v", err)
		}
	})
}

func TestIsIdempotent(t *testing.T) {
	// 详细的方法级校验见 retry_test.go 中的策略单测
	if !isIdempotent(http.MethodGet) || isIdempotent(http.MethodPost) {
		t.Error("幂等性判定异常")
	}
}
