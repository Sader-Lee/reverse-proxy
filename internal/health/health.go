// Package health 实现后端实例的健康检查。
//
// 两条通道共同维护实例的存活状态：
//
//   - 主动：按 interval 定时请求各实例的 health_check.path，仅 2xx 视为健康；
//   - 被动：转发失败由 proxy 通过 ReportFailure 上报，同样计入"连续失败"。
//
// 只有连续失败达到 failure_threshold 才剔除、连续成功达到 success_threshold 才恢复，
// 这种阈值防抖可以避免网络抖动导致实例被反复剔除与恢复（服务震荡）。
package health

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
	"github.com/Sader-Lee/reverse-proxy/internal/config"
)

// maxDrainBody 限制探测响应体的读取量，避免异常响应拖慢检查。
const maxDrainBody = 4 << 10

// Checker 周期性地探测后端实例，并按阈值维护其存活状态。
type Checker struct {
	registry *backend.Registry
	cfg      config.HealthCheck
	client   *http.Client
	logger   *slog.Logger

	mu     sync.Mutex
	states map[*backend.Backend]*streak
}

// streak 记录某个实例连续失败与连续成功的次数。
type streak struct {
	failures  int
	successes int
}

// New 构造健康检查器。cfg.Enabled 为 false 时所有方法都是空操作。
func New(registry *backend.Registry, cfg config.HealthCheck, logger *slog.Logger) *Checker {
	if registry == nil {
		registry = backend.NewRegistry()
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Checker{
		registry: registry,
		cfg:      cfg,
		logger:   logger,
		client: &http.Client{
			Timeout: cfg.Timeout.Duration(),
			// 不跟随重定向：健康检查关心的是该路径本身的响应码
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		states: make(map[*backend.Backend]*streak, registry.Len()),
	}
}

// Enabled 返回健康检查是否启用。
func (c *Checker) Enabled() bool { return c.cfg.Enabled }

// Run 阻塞执行探测循环，直到 ctx 被取消。
// 启动时会先立即探测一次，避免所有实例在首个周期内都停留在"乐观存活"状态。
func (c *Checker) Run(ctx context.Context) {
	if !c.cfg.Enabled {
		return
	}

	interval := c.cfg.Interval.Duration()
	if interval <= 0 {
		c.logger.Warn("健康检查周期非法，检查未启动", "interval", interval)
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	c.CheckOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			c.logger.Debug("健康检查已停止")
			return
		case <-ticker.C:
			c.CheckOnce(ctx)
		}
	}
}

// CheckOnce 对所有实例执行一轮探测。
func (c *Checker) CheckOnce(ctx context.Context) {
	if !c.cfg.Enabled {
		return
	}

	snapshot := c.registry.All()

	var wg sync.WaitGroup
	for _, b := range snapshot {
		wg.Add(1)
		go func(target *backend.Backend) {
			defer wg.Done()
			c.record(target, c.probe(ctx, target))
		}(b)
	}
	wg.Wait()
}

// ReportFailure 由转发链路在请求失败时调用，与主动探测共用同一套连续失败计数。
func (c *Checker) ReportFailure(b *backend.Backend) {
	if !c.cfg.Enabled || b == nil {
		return
	}
	c.record(b, false)
}

// probe 对单个实例发起一次健康探测，仅 2xx 视为健康。
func (c *Checker) probe(ctx context.Context, b *backend.Backend) bool {
	target := b.URL()
	// 后端地址自带基础路径时（如 http://host/base），健康检查路径追加在其后
	target.Path = joinPath(target.Path, c.cfg.Path)
	target.RawPath = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		c.logger.Debug("构造健康检查请求失败", "backend", b.String(), "err", err.Error())
		return false
	}

	resp, err := c.client.Do(req)
	if err != nil {
		c.logger.Debug("健康检查请求失败", "backend", b.String(), "err", err.Error())
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrainBody))
		_ = resp.Body.Close()
	}()

	healthy := resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	if !healthy {
		c.logger.Debug("健康检查返回非 2xx", "backend", b.String(), "status", resp.StatusCode)
	}
	return healthy
}

// record 更新连续计数，并在达到阈值时切换实例的存活状态。
func (c *Checker) record(b *backend.Backend, healthy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.states[b]
	if !ok {
		st = &streak{}
		c.states[b] = st
	}

	if healthy {
		st.failures = 0
		st.successes++
		// SetAlive 的返回值保证只在状态真正翻转时打印一次日志
		if st.successes >= c.cfg.SuccessThreshold && b.SetAlive(true) {
			c.logger.Info("后端恢复可用",
				"backend", b.String(),
				"consecutive_successes", st.successes,
			)
		}
		return
	}

	st.successes = 0
	st.failures++
	if st.failures >= c.cfg.FailureThreshold && b.SetAlive(false) {
		c.logger.Warn("后端被剔除",
			"backend", b.String(),
			"consecutive_failures", st.failures,
		)
	}
}

// joinPath 拼接后端基础路径与健康检查路径。
func joinPath(base, path string) string {
	if path == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}
