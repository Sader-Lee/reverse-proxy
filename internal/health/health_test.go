package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
	"github.com/Sader-Lee/reverse-proxy/internal/config"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testConfig 返回一份便于快速触发的检查配置。
func testConfig() config.HealthCheck {
	return config.HealthCheck{
		Enabled:          true,
		Path:             "/healthz",
		Interval:         config.Duration(10 * time.Millisecond),
		Timeout:          config.Duration(time.Second),
		FailureThreshold: 3,
		SuccessThreshold: 2,
	}
}

// newChecker 用给定地址构造检查器，返回检查器与其注册表。
func newChecker(t *testing.T, cfg config.HealthCheck, rawURLs ...string) (*Checker, *backend.Registry) {
	t.Helper()

	backends := make([]*backend.Backend, 0, len(rawURLs))
	for _, rawURL := range rawURLs {
		b, err := backend.New(rawURL, 1)
		if err != nil {
			t.Fatalf("构造后端 %s 失败: %v", rawURL, err)
		}
		backends = append(backends, b)
	}

	registry := backend.NewRegistry(backends...)
	return New(registry, cfg, discardLogger()), registry
}

// statusServer 返回一个固定状态码的后端。
func statusServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCheckOnceMarksDownAfterFailureThreshold(t *testing.T) {
	srv := statusServer(t, http.StatusInternalServerError)

	cfg := testConfig()
	cfg.FailureThreshold = 3
	checker, registry := newChecker(t, cfg, srv.URL)
	instance := registry.All()[0]

	for i := 1; i < cfg.FailureThreshold; i++ {
		checker.CheckOnce(context.Background())
		if !instance.Alive() {
			t.Fatalf("连续失败 %d 次（阈值 %d）时不应被剔除", i, cfg.FailureThreshold)
		}
	}

	checker.CheckOnce(context.Background())
	if instance.Alive() {
		t.Errorf("连续失败 %d 次后应被剔除", cfg.FailureThreshold)
	}
}

func TestCheckOnceRestoresAfterSuccessThreshold(t *testing.T) {
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.FailureThreshold = 2
	cfg.SuccessThreshold = 2
	checker, registry := newChecker(t, cfg, srv.URL)
	instance := registry.All()[0]

	checker.CheckOnce(context.Background())
	checker.CheckOnce(context.Background())
	if instance.Alive() {
		t.Fatal("连续失败后应被剔除")
	}

	healthy.Store(true)
	checker.CheckOnce(context.Background())
	if instance.Alive() {
		t.Errorf("只成功 1 次（阈值 %d）时不应恢复", cfg.SuccessThreshold)
	}

	checker.CheckOnce(context.Background())
	if !instance.Alive() {
		t.Errorf("连续成功 %d 次后应恢复", cfg.SuccessThreshold)
	}
}

func TestProbeUsesConfiguredPath(t *testing.T) {
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.Path = "/probe/me"
	cfg.SuccessThreshold = 1
	checker, registry := newChecker(t, cfg, srv.URL)
	instance := registry.All()[0]

	checker.CheckOnce(context.Background())

	if got, _ := gotPath.Load().(string); got != "/probe/me" {
		t.Errorf("探测路径 = %q, 期望 \"/probe/me\"", got)
	}
	if !instance.Alive() {
		t.Error("200 应视为健康")
	}
}

func TestProbeAppendsToBackendBasePath(t *testing.T) {
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.SuccessThreshold = 1
	checker, _ := newChecker(t, cfg, srv.URL+"/base")

	checker.CheckOnce(context.Background())

	if got, _ := gotPath.Load().(string); got != "/base/healthz" {
		t.Errorf("探测路径 = %q, 期望 \"/base/healthz\"", got)
	}
}

func TestProbeTreatsNon2xxAsFailure(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		wantAlive bool
	}{
		{name: "204 视为健康", status: http.StatusNoContent, wantAlive: true},
		{name: "301 不算健康", status: http.StatusMovedPermanently, wantAlive: false},
		{name: "404 不算健康", status: http.StatusNotFound, wantAlive: false},
		{name: "500 不算健康", status: http.StatusInternalServerError, wantAlive: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := statusServer(t, tc.status)

			cfg := testConfig()
			// 阈值置 1，一次探测即可判定
			cfg.FailureThreshold = 1
			cfg.SuccessThreshold = 1
			checker, registry := newChecker(t, cfg, srv.URL)
			instance := registry.All()[0]

			checker.CheckOnce(context.Background())

			if instance.Alive() != tc.wantAlive {
				t.Errorf("状态码 %d 时 Alive = %v, 期望 %v", tc.status, instance.Alive(), tc.wantAlive)
			}
		})
	}
}

func TestProbeTimeoutCountsAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.Timeout = config.Duration(20 * time.Millisecond)
	cfg.FailureThreshold = 1
	checker, registry := newChecker(t, cfg, srv.URL)
	instance := registry.All()[0]

	checker.CheckOnce(context.Background())

	if instance.Alive() {
		t.Error("探测超时应视为失败")
	}
}

func TestReportFailureSharesCounterWithProbe(t *testing.T) {
	srv := statusServer(t, http.StatusOK)

	cfg := testConfig()
	cfg.FailureThreshold = 3
	cfg.SuccessThreshold = 2
	checker, registry := newChecker(t, cfg, srv.URL)
	instance := registry.All()[0]

	// 被动失败累计到阈值即剔除，无需等待下一轮探测
	checker.ReportFailure(instance)
	checker.ReportFailure(instance)
	if !instance.Alive() {
		t.Fatal("连续失败 2 次（阈值 3）时不应被剔除")
	}
	checker.ReportFailure(instance)
	if instance.Alive() {
		t.Fatal("连续失败 3 次后应被剔除")
	}

	// 探测成功会清零被动失败计数
	checker.CheckOnce(context.Background())
	if instance.Alive() {
		t.Error("成功 1 次（阈值 2）时不应恢复")
	}
	checker.CheckOnce(context.Background())
	if !instance.Alive() {
		t.Error("连续成功 2 次后应恢复")
	}

	// 恢复后：2 次被动失败 + 1 次探测成功 + 1 次被动失败 = 1 次连续失败
	checker.ReportFailure(instance)
	checker.ReportFailure(instance)
	checker.CheckOnce(context.Background())
	checker.ReportFailure(instance)
	if !instance.Alive() {
		t.Error("探测成功应清零连续失败计数")
	}
}

func TestDisabledCheckerIsNoop(t *testing.T) {
	srv := statusServer(t, http.StatusInternalServerError)

	cfg := testConfig()
	cfg.Enabled = false
	cfg.FailureThreshold = 1
	checker, registry := newChecker(t, cfg, srv.URL)
	instance := registry.All()[0]

	if checker.Enabled() {
		t.Error("Enabled 应为 false")
	}

	checker.CheckOnce(context.Background())
	for i := 0; i < 5; i++ {
		checker.ReportFailure(instance)
	}

	if !instance.Alive() {
		t.Error("健康检查关闭时不应改动实例状态")
	}
}

func TestReportFailureIgnoresNil(t *testing.T) {
	checker, _ := newChecker(t, testConfig(), "http://127.0.0.1:9001")
	checker.ReportFailure(nil) // 不应 panic
}

func TestRunProbesAndStopsOnContextCancel(t *testing.T) {
	srv := statusServer(t, http.StatusInternalServerError)

	cfg := testConfig()
	cfg.Interval = config.Duration(10 * time.Millisecond)
	cfg.FailureThreshold = 2
	checker, registry := newChecker(t, cfg, srv.URL)
	instance := registry.All()[0]

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		checker.Run(ctx)
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for instance.Alive() {
		select {
		case <-deadline:
			cancel()
			t.Fatal("超时：探测循环未按阈值剔除实例")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("ctx 取消后 Run 未退出")
	}
}

func TestCheckerIsConcurrencySafe(t *testing.T) {
	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	cfg := testConfig()
	cfg.FailureThreshold = 2
	cfg.SuccessThreshold = 2
	checker, registry := newChecker(t, cfg, srv.URL, srv.URL, srv.URL)

	instances := registry.All()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				checker.CheckOnce(context.Background())
				healthy.Store(j%2 == 0)
				for _, b := range instances {
					checker.ReportFailure(b)
					_ = b.Alive()
				}
			}
		}()
	}
	wg.Wait()
}
