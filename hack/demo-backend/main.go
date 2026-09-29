// Command demo-backend 是用于本地端到端验证的极简后端服务。
//
// 用法：
//
//	go run ./hack/demo-backend -addr :9001 -name backend-a
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
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		echo(*name, w, r)
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

// echo 回显请求信息，并支持 sleep / status 两个调试参数。
func echo(name string, w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if d := query.Get("sleep"); d != "" {
		if dur, err := time.ParseDuration(d); err == nil {
			fmt.Fprintf(os.Stderr, "[%s] %s %s 延迟 %s 后响应\n", name, r.Method, r.RequestURI, dur)
			time.Sleep(dur)
		}
	}

	if s := query.Get("status"); s != "" {
		code, err := strconv.Atoi(s)
		if err != nil || code < 100 || code > 599 {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
		fmt.Fprintf(os.Stderr, "[%s] %s %s -> %d\n", name, r.Method, r.RequestURI, code)
		w.WriteHeader(code)
		_, _ = fmt.Fprintf(w, "%s: %d\n", name, code)
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
