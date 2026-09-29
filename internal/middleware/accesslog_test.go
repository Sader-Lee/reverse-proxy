package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/Sader-Lee/reverse-proxy/internal/proxy"
)

// newTestRouter 构造只挂载访问日志中间件的 Gin 引擎。
func newTestRouter(t *testing.T, buf *bytes.Buffer, handler gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	logger := slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	router := gin.New()
	router.Use(AccessLog(logger))
	router.GET("/hello", handler)
	router.GET("/boom", handler)
	return router
}

// decodeRecord 解析缓冲区中最后一条 JSON 日志。
func decodeRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	raw := lines[len(lines)-1]

	var record map[string]any
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		t.Fatalf("解析访问日志失败: %v, 原始内容: %s", err, raw)
	}
	return record
}

func TestAccessLogRecordsRequestFields(t *testing.T) {
	var buf bytes.Buffer
	router := newTestRouter(t, &buf, func(c *gin.Context) {
		c.Set(proxy.CtxKeyUpstream, "http://127.0.0.1:9001")
		c.String(http.StatusOK, "hello world")
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/hello?page=2", nil)
	req.Header.Set("User-Agent", "curl/8.0")
	router.ServeHTTP(httptest.NewRecorder(), req)

	record := decodeRecord(t, &buf)

	if record["msg"] != "access" {
		t.Errorf("msg = %v, 期望 \"access\"", record["msg"])
	}
	if record["level"] != "INFO" {
		t.Errorf("level = %v, 期望 INFO", record["level"])
	}
	if record["method"] != "GET" {
		t.Errorf("method = %v, 期望 GET", record["method"])
	}
	if record["path"] != "/hello" {
		t.Errorf("path = %v, 期望 /hello", record["path"])
	}
	if record["query"] != "page=2" {
		t.Errorf("query = %v, 期望 page=2", record["query"])
	}
	if record["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, 期望 200", record["status"])
	}
	if record["bytes"] != float64(len("hello world")) {
		t.Errorf("bytes = %v, 期望 %d", record["bytes"], len("hello world"))
	}
	if record["upstream"] != "http://127.0.0.1:9001" {
		t.Errorf("upstream = %v, 期望 http://127.0.0.1:9001", record["upstream"])
	}
	if record["user_agent"] != "curl/8.0" {
		t.Errorf("user_agent = %v, 期望 curl/8.0", record["user_agent"])
	}
	if _, ok := record["latency_ms"]; !ok {
		t.Error("缺少 latency_ms 字段")
	}
	if ip, _ := record["client_ip"].(string); ip == "" {
		t.Error("client_ip 不应为空")
	}
}

func TestAccessLogUsesWarnLevelForErrors(t *testing.T) {
	var buf bytes.Buffer
	router := newTestRouter(t, &buf, func(c *gin.Context) {
		c.String(http.StatusServiceUnavailable, "unavailable")
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/boom", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)

	record := decodeRecord(t, &buf)

	if record["level"] != "WARN" {
		t.Errorf("level = %v, 期望 5xx 时输出 WARN", record["level"])
	}
	if record["status"] != float64(http.StatusServiceUnavailable) {
		t.Errorf("status = %v, 期望 503", record["status"])
	}
	if _, ok := record["upstream"]; ok {
		t.Error("未设置转发目标时不应输出 upstream 字段")
	}
}

func TestAccessLogRecordsEscapedPathWhenDiffers(t *testing.T) {
	var buf bytes.Buffer
	router := newTestRouter(t, &buf, func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/hello", nil)
	req.URL.Path = "/files/a/b/c.txt"
	req.URL.RawPath = "/files/a%2Fb/c.txt"
	router.ServeHTTP(httptest.NewRecorder(), req)

	record := decodeRecord(t, &buf)
	if record["escaped_path"] != "/files/a%2Fb/c.txt" {
		t.Errorf("escaped_path = %v, 期望 /files/a%%2Fb/c.txt", record["escaped_path"])
	}
}

func TestAccessLogOmitsEscapedPathWhenSame(t *testing.T) {
	var buf bytes.Buffer
	router := newTestRouter(t, &buf, func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/hello", nil)
	router.ServeHTTP(httptest.NewRecorder(), req)

	record := decodeRecord(t, &buf)
	if _, ok := record["escaped_path"]; ok {
		t.Error("路径未被编码时不应输出 escaped_path 字段")
	}
}

func TestAccessLogEscapesQueryInJSON(t *testing.T) {
	var buf bytes.Buffer
	router := newTestRouter(t, &buf, func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	// 直接构造含双引号的 RawQuery：访问日志是单行 JSON，不能因为引号换行
	req := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/hello", nil)
	req.URL.RawQuery = `q="x"`
	router.ServeHTTP(httptest.NewRecorder(), req)

	// 单行 JSON 是"每条日志一行"的基础保证，含引号的查询串不能破坏结构
	if lines := strings.Count(strings.TrimSpace(buf.String()), "\n"); lines != 0 {
		t.Errorf("访问日志应为单行 JSON，实际包含 %d 个换行", lines)
	}

	record := decodeRecord(t, &buf)
	if record["query"] != `q="x"` {
		t.Errorf("query = %v, 期望 q=\"x\"", record["query"])
	}
}
