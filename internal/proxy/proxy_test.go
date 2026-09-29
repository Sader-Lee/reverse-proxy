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

func TestNewRejectsInvalidTarget(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		wantErr string
	}{
		{name: "空地址", target: "", wantErr: "scheme"},
		{name: "非法 scheme", target: "tcp://127.0.0.1:9001", wantErr: "scheme"},
		{name: "缺少主机名", target: "http:///api", wantErr: "缺少主机名"},
		{name: "包含非法字符", target: "http://127.0.0.1:9001/%zz", wantErr: "解析后端地址"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.target, discardLogger())
			if err == nil {
				t.Fatalf("target=%q 期望报错，实际通过", tc.target)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息未包含 %q，实际: %v", tc.wantErr, err)
			}
		})
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

	handler, err := New(upstream.URL, discardLogger())
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodPost, "http://proxy.example.com/api/users?page=2", strings.NewReader(`{"name":"lee"}`))
	req.Header.Set("Content-Type", "application/json")
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
}

func TestPreservesEncodedPath(t *testing.T) {
	var gotRequestURI string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRequestURI = r.RequestURI
	}))
	defer upstream.Close()

	handler, err := New(upstream.URL, discardLogger())
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/files/a%2Fb/c.txt", nil)
	w := newRecorder()
	router.ServeHTTP(w, req)

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

	handler, err := New(upstream.URL+"/base", discardLogger())
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}
	router := newTestRouter(t, handler)

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/svc", nil)
	router.ServeHTTP(newRecorder(), req)

	if gotPath != "/base/svc" {
		t.Errorf("后端收到路径 = %q, 期望 \"/base/svc\"", gotPath)
	}
}

func TestReturnsBadGatewayWhenUpstreamUnreachable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := upstream.URL
	upstream.Close() // 立刻关闭，制造必然连不上的后端

	handler, err := New(target, discardLogger())
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}
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
}

func TestHandleWritesUpstreamToContext(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler, err := New(upstream.URL, discardLogger())
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.UseRawPath = true
	router.Any("/*path", func(c *gin.Context) {
		handler.Handle(c)
		if got := c.GetString(CtxKeyUpstream); got != handler.Target() {
			t.Errorf("上下文中的 upstream = %q, 期望 %q", got, handler.Target())
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

	handler, err := New(upstream.URL, discardLogger())
	if err != nil {
		t.Fatalf("构造处理器失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/plain", nil).WithContext(context.Background())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK || w.Body.String() != "plain" {
		t.Errorf("直接使用 http.Handler 时转发失败: code=%d body=%q", w.Code, w.Body.String())
	}
}
