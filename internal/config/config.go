// Package config 负责代理服务的配置加载、默认值填充与合法性校验。
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Strategy 表示负载均衡策略。
type Strategy string

// 支持的负载均衡策略。
const (
	StrategyRoundRobin         Strategy = "round-robin"
	StrategyRandom             Strategy = "random"
	StrategyWeightedRoundRobin Strategy = "weighted-round-robin"
)

// DefaultConfigPath 是未显式指定配置路径时使用的默认位置。
const DefaultConfigPath = "configs/config.yaml"

// Config 是代理服务的完整配置。
type Config struct {
	Listen      string      `yaml:"listen" json:"listen"`
	Strategy    Strategy    `yaml:"strategy" json:"strategy"`
	Backends    []Backend   `yaml:"backends" json:"backends"`
	HealthCheck HealthCheck `yaml:"health_check" json:"health_check"`
	Retry       Retry       `yaml:"retry" json:"retry"`
	RateLimit   RateLimit   `yaml:"rate_limit" json:"rate_limit"`
	Logging     Logging     `yaml:"logging" json:"logging"`
	Timeouts    Timeouts    `yaml:"timeouts" json:"timeouts"`
}

// Backend 描述一个后端实例。
type Backend struct {
	URL    string `yaml:"url" json:"url"`
	Weight int    `yaml:"weight" json:"weight"`
}

// HealthCheck 描述健康检查行为。
type HealthCheck struct {
	Enabled          bool     `yaml:"enabled" json:"enabled"`
	Path             string   `yaml:"path" json:"path"`
	Interval         Duration `yaml:"interval" json:"interval"`
	Timeout          Duration `yaml:"timeout" json:"timeout"`
	FailureThreshold int      `yaml:"failure_threshold" json:"failure_threshold"`
	SuccessThreshold int      `yaml:"success_threshold" json:"success_threshold"`
}

// Retry 描述超时与重试行为。
type Retry struct {
	MaxAttempts        int      `yaml:"max_attempts" json:"max_attempts"`
	PerTryTimeout      Duration `yaml:"per_try_timeout" json:"per_try_timeout"`
	RetryOnStatus      []int    `yaml:"retry_on_status" json:"retry_on_status"`
	RetryNonIdempotent bool     `yaml:"retry_non_idempotent" json:"retry_non_idempotent"`
}

// RateLimit 描述令牌桶限流配置。
type RateLimit struct {
	Enabled bool    `yaml:"enabled" json:"enabled"`
	RPS     float64 `yaml:"rps" json:"rps"`
	Burst   int     `yaml:"burst" json:"burst"`
	By      string  `yaml:"by" json:"by"`
}

// Logging 描述日志配置。
type Logging struct {
	Level     string `yaml:"level" json:"level"`
	Format    string `yaml:"format" json:"format"`
	AccessLog bool   `yaml:"access_log" json:"access_log"`
}

// Timeouts 描述 HTTP 服务端超时配置。
type Timeouts struct {
	ReadHeader Duration `yaml:"read_header" json:"read_header"`
	Write      Duration `yaml:"write" json:"write"`
	Idle       Duration `yaml:"idle" json:"idle"`
}

// Duration 包装 time.Duration，使其在 YAML 中支持 "5s"、"1m" 写法，
// 同时兼容纯数字（按秒解释）。
type Duration time.Duration

// UnmarshalYAML 实现 yaml.Unmarshaler。
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	s := strings.TrimSpace(node.Value)
	if s == "" {
		*d = 0
		return nil
	}
	if secs, err := strconv.Atoi(s); err == nil {
		*d = Duration(time.Duration(secs) * time.Second)
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("无法解析时长 %q（示例：3s / 500ms / 1m）: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML 输出可读的时长字符串。
func (d Duration) MarshalYAML() (any, error) {
	return d.Duration().String(), nil
}

// MarshalJSON 输出可读的时长字符串。
func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(d.Duration().String())), nil
}

// Duration 返回标准库时长。
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// Default 返回一份带默认值的配置。
func Default() *Config {
	return &Config{
		Listen:   ":8080",
		Strategy: StrategyRoundRobin,
		HealthCheck: HealthCheck{
			Enabled:          true,
			Path:             "/healthz",
			Interval:         Duration(5 * time.Second),
			Timeout:          Duration(2 * time.Second),
			FailureThreshold: 3,
			SuccessThreshold: 2,
		},
		Retry: Retry{
			MaxAttempts:   3,
			PerTryTimeout: Duration(3 * time.Second),
			RetryOnStatus: []int{502, 503, 504},
		},
		RateLimit: RateLimit{
			Enabled: false,
			RPS:     1000,
			Burst:   200,
			By:      "ip",
		},
		Logging: Logging{
			Level:     "info",
			Format:    "json",
			AccessLog: true,
		},
		Timeouts: Timeouts{
			ReadHeader: Duration(10 * time.Second),
			Write:      0,
			Idle:       Duration(90 * time.Second),
		},
	}
}

// Load 从指定路径读取 YAML 配置，缺失字段沿用默认值，并执行校验。
// 返回的配置已完成规范化（如补全 weight、清理 URL 尾部斜杠）。
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}

	cfg := Default()
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}

	cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置文件 %s 校验失败: %w", path, err)
	}
	return cfg, nil
}

// normalize 修正可自动兼容的配置项，并去除用户输入里的噪音。
func (c *Config) normalize() {
	c.Listen = strings.TrimSpace(c.Listen)
	c.Strategy = Strategy(strings.ToLower(strings.TrimSpace(string(c.Strategy))))
	c.Logging.Level = strings.ToLower(strings.TrimSpace(c.Logging.Level))
	c.Logging.Format = strings.ToLower(strings.TrimSpace(c.Logging.Format))
	c.RateLimit.By = strings.ToLower(strings.TrimSpace(c.RateLimit.By))

	for i := range c.Backends {
		c.Backends[i].URL = strings.TrimRight(strings.TrimSpace(c.Backends[i].URL), "/")
		if c.Backends[i].Weight <= 0 {
			c.Backends[i].Weight = 1
		}
	}
	if c.HealthCheck.Path == "" {
		c.HealthCheck.Path = "/healthz"
	}
	if !strings.HasPrefix(c.HealthCheck.Path, "/") {
		c.HealthCheck.Path = "/" + c.HealthCheck.Path
	}
}

// Validate 校验配置的合法性，返回全部问题的聚合错误。
func (c *Config) Validate() error {
	var errs []error

	if c.Listen == "" {
		errs = append(errs, errors.New("listen 不能为空"))
	}

	switch c.Strategy {
	case StrategyRoundRobin, StrategyRandom, StrategyWeightedRoundRobin:
	default:
		errs = append(errs, fmt.Errorf("不支持的 strategy %q（可选：%s / %s / %s）",
			c.Strategy, StrategyRoundRobin, StrategyRandom, StrategyWeightedRoundRobin))
	}

	if len(c.Backends) == 0 {
		errs = append(errs, errors.New("backends 至少需要配置一个后端实例"))
	}
	seen := make(map[string]int, len(c.Backends))
	for i, b := range c.Backends {
		if b.URL == "" {
			errs = append(errs, fmt.Errorf("backends[%d].url 不能为空", i))
			continue
		}
		u, err := url.Parse(b.URL)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("backends[%d].url %q 无法解析: %w", i, b.URL, err))
		case u.Scheme != "http" && u.Scheme != "https":
			errs = append(errs, fmt.Errorf("backends[%d].url %q 的 scheme 必须是 http 或 https", i, b.URL))
		case u.Host == "":
			errs = append(errs, fmt.Errorf("backends[%d].url %q 缺少主机名", i, b.URL))
		}
		if prev, ok := seen[b.URL]; ok {
			errs = append(errs, fmt.Errorf("backends[%d].url %q 与 backends[%d] 重复", i, b.URL, prev))
		}
		seen[b.URL] = i
	}

	if c.HealthCheck.Enabled {
		if c.HealthCheck.Interval <= 0 {
			errs = append(errs, errors.New("health_check.interval 必须大于 0"))
		}
		if c.HealthCheck.Timeout <= 0 {
			errs = append(errs, errors.New("health_check.timeout 必须大于 0"))
		}
		if c.HealthCheck.FailureThreshold < 1 {
			errs = append(errs, errors.New("health_check.failure_threshold 至少为 1"))
		}
		if c.HealthCheck.SuccessThreshold < 1 {
			errs = append(errs, errors.New("health_check.success_threshold 至少为 1"))
		}
	}

	if c.Retry.MaxAttempts < 1 {
		errs = append(errs, errors.New("retry.max_attempts 至少为 1"))
	}
	if c.Retry.PerTryTimeout <= 0 {
		errs = append(errs, errors.New("retry.per_try_timeout 必须大于 0"))
	}
	for _, code := range c.Retry.RetryOnStatus {
		if code < 100 || code > 599 {
			errs = append(errs, fmt.Errorf("retry.retry_on_status 含非法状态码 %d", code))
		}
	}

	if c.RateLimit.Enabled {
		if c.RateLimit.RPS <= 0 {
			errs = append(errs, errors.New("rate_limit.rps 必须大于 0"))
		}
		if c.RateLimit.Burst < 1 {
			errs = append(errs, errors.New("rate_limit.burst 至少为 1"))
		}
		switch c.RateLimit.By {
		case "ip", "global":
		default:
			errs = append(errs, fmt.Errorf("不支持的 rate_limit.by %q（可选：ip / global）", c.RateLimit.By))
		}
	}

	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("不支持的 logging.level %q（可选：debug / info / warn / error）", c.Logging.Level))
	}
	switch c.Logging.Format {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("不支持的 logging.format %q（可选：json / text）", c.Logging.Format))
	}

	if c.Timeouts.ReadHeader <= 0 {
		errs = append(errs, errors.New("timeouts.read_header 必须大于 0"))
	}
	if c.Timeouts.Idle <= 0 {
		errs = append(errs, errors.New("timeouts.idle 必须大于 0"))
	}

	return errors.Join(errs...)
}

// SlogLevel 将配置的日志级别映射为 slog.Level。
func (l Logging) SlogLevel() slog.Level {
	switch l.Level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// URLs 返回全部后端地址，顺序与配置一致。
func (c *Config) URLs() []string {
	urls := make([]string, 0, len(c.Backends))
	for _, b := range c.Backends {
		urls = append(urls, b.URL)
	}
	return urls
}
