// Command demo-backend 是用于本地端到端验证的极简后端服务。
//
// 用法：
//
//	go run ./hack/demo-backend -addr :9001 -name backend-a
//	go run ./hack/demo-backend -addr :9001 -name backend-a -delay 2s   # 慢实例（验证超时与重试）
//	go run ./hack/demo-backend -addr :9001 -name backend-a -status 503 # 故障实例（验证重试与健康检查）
//
// 支持的请求参数：
//
//	sleep=2s     延迟指定时长后响应（验证超时与重试）
//	status=503   返回指定状态码（验证重试与健康检查）
//	body=xxx     在响应中附带自定义内容
//
// 除 /healthz 外的所有请求都会以 JSON 回显实例名、方法、路径、请求体与转发头，
// 便于确认请求最终落到了哪个实例。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// maxEchoBody 限制回显的请求体大小，避免演示时被大请求拖垮。
const maxEchoBody = 1 << 20

func main() {
	addr := flag.String("addr", ":9001", "监听地址")
	name := flag.String("name", "backend", "实例名，用于回显与日志前缀")
	delay := flag.Duration("delay", 0, "每个请求固定延迟该时长后响应（模拟慢实例）")
	status := flag.Int("status", 0, "所有请求固定返回该状态码（模拟故障实例，0 表示正常）")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		echo(*name, *delay, *status, w, r)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	fmt.Fprintf(os.Stderr, "[%s] 监听 %s\n", *name, *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("[%s] 服务退出: %v", *name, err)
	}
}

// echo 回显请求信息，并支持 delay/status/sleep 等调试开关。
func echo(name string, fixedDelay time.Duration, fixedStatus int, w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	delay := fixedDelay
	if d := query.Get("sleep"); d != "" {
		if parsed, err := time.ParseDuration(d); err == nil {
			delay = parsed
		}
	}
	if delay > 0 {
		fmt.Fprintf(os.Stderr, "[%s] %s %s 延迟 %s 后响应\n", name, r.Method, r.RequestURI, delay)
		time.Sleep(delay)
	}

	status := fixedStatus
	if s := query.Get("status"); s != "" {
		parsed, err := strconv.Atoi(s)
		if err != nil || parsed < 100 || parsed > 599 {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
		status = parsed
	}
	if status != 0 {
		fmt.Fprintf(os.Stderr, "[%s] %s %s -> %d\n", name, r.Method, r.RequestURI, status)
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "%s: %d\n", name, status)
		return
	}

	body, _ := io.ReadAll(io.LimitReader(r.Body, maxEchoBody))

	payload := map[string]any{
		"instance":          name,
		"method":            r.Method,
		"path":              r.URL.Path,
		"escaped_path":      r.URL.EscapedPath(),
		"request_uri":       r.RequestURI,
		"raw_query":         r.URL.RawQuery,
		"host":              r.Host,
		"remote_addr":       r.RemoteAddr,
		"x_forwarded_for":   r.Header.Get("X-Forwarded-For"),
		"x_forwarded_proto": r.Header.Get("X-Forwarded-Proto"),
		"x_forwarded_host":  r.Header.Get("X-Forwarded-Host"),
	}
	if len(body) > 0 {
		payload["request_body"] = string(body)
	}

	fmt.Fprintf(os.Stderr, "[%s] %s %s -> 200\n", name, r.Method, r.RequestURI)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Instance", name)
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(payload)
}
