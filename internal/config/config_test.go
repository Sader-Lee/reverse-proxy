package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig 在临时目录写入一份 YAML 配置并返回其路径。
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	return path
}

func TestLoadExampleConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "configs", "config.example.yaml"))
	if err != nil {
		t.Fatalf("加载示例配置失败: %v", err)
	}

	if cfg.Listen != ":8080" {
		t.Errorf("listen = %q, 期望 \":8080\"", cfg.Listen)
	}
	if cfg.Strategy != StrategyWeightedRoundRobin {
		t.Errorf("strategy = %q, 期望 %q", cfg.Strategy, StrategyWeightedRoundRobin)
	}
	if len(cfg.Backends) != 2 {
		t.Fatalf("backends 数量 = %d, 期望 2", len(cfg.Backends))
	}
	if cfg.Backends[0].Weight != 3 || cfg.Backends[1].Weight != 1 {
		t.Errorf("权重 = [%d %d], 期望 [3 1]", cfg.Backends[0].Weight, cfg.Backends[1].Weight)
	}
	if got := cfg.HealthCheck.Interval.Duration(); got != 5*time.Second {
		t.Errorf("health_check.interval = %v, 期望 5s", got)
	}
	if cfg.HealthCheck.FailureThreshold != 3 || cfg.HealthCheck.SuccessThreshold != 2 {
		t.Errorf("健康检查阈值 = [%d %d], 期望 [3 2]", cfg.HealthCheck.FailureThreshold, cfg.HealthCheck.SuccessThreshold)
	}
	if cfg.Retry.MaxAttempts != 3 {
		t.Errorf("retry.max_attempts = %d, 期望 3", cfg.Retry.MaxAttempts)
	}
	if got := cfg.Retry.PerTryTimeout.Duration(); got != 3*time.Second {
		t.Errorf("retry.per_try_timeout = %v, 期望 3s", got)
	}
	if len(cfg.Retry.RetryOnStatus) != 3 {
		t.Errorf("retry_on_status = %v, 期望含 3 个状态码", cfg.Retry.RetryOnStatus)
	}
	if cfg.RateLimit.Enabled {
		t.Error("rate_limit.enabled 期望为 false")
	}
	if got := cfg.Timeouts.Idle.Duration(); got != 90*time.Second {
		t.Errorf("timeouts.idle = %v, 期望 90s", got)
	}
}

func TestLoadAppliesDefaultsForMissingFields(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
backends:
  - url: "http://127.0.0.1:9001"
`))
	if err != nil {
		t.Fatalf("加载最小配置失败: %v", err)
	}

	def := Default()
	if cfg.Listen != def.Listen {
		t.Errorf("listen = %q, 期望沿用默认值 %q", cfg.Listen, def.Listen)
	}
	if cfg.Strategy != StrategyRoundRobin {
		t.Errorf("strategy = %q, 期望默认 %q", cfg.Strategy, StrategyRoundRobin)
	}
	if cfg.HealthCheck.Path != "/healthz" {
		t.Errorf("health_check.path = %q, 期望默认 \"/healthz\"", cfg.HealthCheck.Path)
	}
	if !cfg.HealthCheck.Enabled {
		t.Error("health_check.enabled 期望默认开启")
	}
	if cfg.Retry.MaxAttempts != def.Retry.MaxAttempts {
		t.Errorf("retry.max_attempts = %d, 期望默认 %d", cfg.Retry.MaxAttempts, def.Retry.MaxAttempts)
	}
	if cfg.Logging.Level != "info" || cfg.Logging.Format != "json" {
		t.Errorf("logging = %+v, 期望默认 info/json", cfg.Logging)
	}
}

func TestLoadNormalizes(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
listen: "  :8080  "
strategy: "  WEIGHTED-ROUND-ROBIN "
backends:
  - url: " http://127.0.0.1:9001/ "
    weight: 0
  - url: "http://127.0.0.1:9002"
    weight: -5
health_check:
  path: "healthz"
logging:
  level: "  DEBUG "
  format: "TEXT"
rate_limit:
  enabled: true
  rps: 10
  burst: 5
  by: " IP "
`))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	if cfg.Listen != ":8080" {
		t.Errorf("listen 未 trim: %q", cfg.Listen)
	}
	if cfg.Strategy != StrategyWeightedRoundRobin {
		t.Errorf("strategy 未归一化: %q", cfg.Strategy)
	}
	if cfg.Backends[0].URL != "http://127.0.0.1:9001" {
		t.Errorf("后端 URL 未清理尾部斜杠与空格: %q", cfg.Backends[0].URL)
	}
	if cfg.Backends[0].Weight != 1 || cfg.Backends[1].Weight != 1 {
		t.Errorf("非法权重未被归一为 1: [%d %d]", cfg.Backends[0].Weight, cfg.Backends[1].Weight)
	}
	if cfg.HealthCheck.Path != "/healthz" {
		t.Errorf("健康检查路径未补前导斜杠: %q", cfg.HealthCheck.Path)
	}
	if cfg.Logging.Level != "debug" || cfg.Logging.Format != "text" {
		t.Errorf("logging 未归一化: %+v", cfg.Logging)
	}
	if cfg.RateLimit.By != "ip" {
		t.Errorf("rate_limit.by 未归一化: %q", cfg.RateLimit.By)
	}
}

func TestDurationParsing(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
backends:
  - url: "http://127.0.0.1:9001"
retry:
  per_try_timeout: "500ms"
health_check:
  interval: "2"      # 纯数字按秒解释
  timeout: "1m"
timeouts:
  write: ""          # 空值视为 0（不限制）
`))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	cases := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"per_try_timeout=500ms", cfg.Retry.PerTryTimeout.Duration(), 500 * time.Millisecond},
		{"interval=2(秒)", cfg.HealthCheck.Interval.Duration(), 2 * time.Second},
		{"timeout=1m", cfg.HealthCheck.Timeout.Duration(), time.Minute},
		{"write=空", cfg.Timeouts.Write.Duration(), 0},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestLoadRejectsInvalidConfig(t *testing.T) {
	baseBackends := `
backends:
  - url: "http://127.0.0.1:9001"
`

	tests := []struct {
		name        string
		yaml        string
		wantContain string
	}{
		{
			name:        "未知策略",
			yaml:        baseBackends + "strategy: \"least-conn\"\n",
			wantContain: "不支持的 strategy",
		},
		{
			name:        "缺少后端",
			yaml:        "listen: \":8080\"\n",
			wantContain: "至少需要配置一个后端实例",
		},
		{
			name: "后端 URL 重复",
			yaml: `
backends:
  - url: "http://127.0.0.1:9001"
  - url: "http://127.0.0.1:9001/"
`,
			wantContain: "重复",
		},
		{
			name: "后端 scheme 非法",
			yaml: `
backends:
  - url: "tcp://127.0.0.1:9001"
`,
			wantContain: "scheme 必须是 http 或 https",
		},
		{
			name: "后端缺少主机名",
			yaml: `
backends:
  - url: "http:///api"
`,
			wantContain: "缺少主机名",
		},
		{
			name:        "时长格式非法",
			yaml:        baseBackends + "retry:\n  per_try_timeout: \"3 秒\"\n",
			wantContain: "无法解析时长",
		},
		{
			name:        "最大尝试次数为 0",
			yaml:        baseBackends + "retry:\n  max_attempts: 0\n",
			wantContain: "max_attempts",
		},
		{
			name:        "健康检查阈值非法",
			yaml:        baseBackends + "health_check:\n  failure_threshold: 0\n",
			wantContain: "failure_threshold",
		},
		{
			name:        "日志级别非法",
			yaml:        baseBackends + "logging:\n  level: verbose\n",
			wantContain: "logging.level",
		},
		{
			name:        "日志格式非法",
			yaml:        baseBackends + "logging:\n  format: xml\n",
			wantContain: "logging.format",
		},
		{
			name:        "限流域非法",
			yaml:        baseBackends + "rate_limit:\n  enabled: true\n  by: header\n",
			wantContain: "rate_limit.by",
		},
		{
			name:        "限流速率非法",
			yaml:        baseBackends + "rate_limit:\n  enabled: true\n  rps: -1\n",
			wantContain: "rate_limit.rps",
		},
		{
			name:        "重试状态码非法",
			yaml:        baseBackends + "retry:\n  retry_on_status: [99]\n",
			wantContain: "非法状态码",
		},
		{
			name:        "监听地址为空",
			yaml:        baseBackends + "listen: \"\"\n",
			wantContain: "listen 不能为空",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.yaml))
			if err == nil {
				t.Fatalf("期望校验失败，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantContain) {
				t.Errorf("错误信息未包含 %q，实际: %v", tc.wantContain, err)
			}
		})
	}
}

func TestValidateAggregatesAllErrors(t *testing.T) {
	cfg := Default()
	cfg.Listen = ""
	cfg.Strategy = "unknown"
	cfg.Retry.MaxAttempts = 0

	err := cfg.Validate()
	if err == nil {
		t.Fatal("期望返回错误")
	}
	for _, want := range []string{"listen 不能为空", "不支持的 strategy", "max_attempts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("聚合错误中缺失 %q，实际: %v", want, err)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "not-exist.yaml"))
	if err == nil {
		t.Fatal("期望读取失败")
	}
	if !strings.Contains(err.Error(), "读取配置文件") {
		t.Errorf("错误信息不符合预期: %v", err)
	}
}

func TestSlogLevelMapping(t *testing.T) {
	cases := map[string]string{
		"debug": "DEBUG",
		"info":  "INFO",
		"warn":  "WARN",
		"error": "ERROR",
	}
	for level, want := range cases {
		got := Logging{Level: level}.SlogLevel().String()
		if got != want {
			t.Errorf("level %q -> %s, 期望 %s", level, got, want)
		}
	}
}

func TestURLsPreservesOrder(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
backends:
  - url: "http://127.0.0.1:9001"
  - url: "http://127.0.0.1:9002"
  - url: "http://127.0.0.1:9003"
`))
	if err != nil {
		t.Fatalf("加载配置失败: %v", err)
	}

	urls := cfg.URLs()
	if len(urls) != 3 {
		t.Fatalf("URLs 长度 = %d, 期望 3", len(urls))
	}
	if urls[0] != "http://127.0.0.1:9001" || urls[2] != "http://127.0.0.1:9003" {
		t.Errorf("URLs 顺序错误: %v", urls)
	}
}
