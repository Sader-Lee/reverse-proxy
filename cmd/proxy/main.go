// Command proxy 是反向代理与负载均衡器的入口。
//
// 支持的最小 CLI：
//
//	-config   指定配置文件（必选，其他参数都是可选的）
//	-check    只校验配置并退出
//	-version  打印版本信息并退出
//	-listen   覆盖配置中的监听地址
//
// 其余可调项（后端列表、策略、健康检查、重试等）一律由配置文件提供，
// 避免同一份配置出现两个来源。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
	"github.com/Sader-Lee/reverse-proxy/internal/balancer"
	"github.com/Sader-Lee/reverse-proxy/internal/config"
	"github.com/Sader-Lee/reverse-proxy/internal/health"
	"github.com/Sader-Lee/reverse-proxy/internal/middleware"
	"github.com/Sader-Lee/reverse-proxy/internal/proxy"
)

func main() {
	opts, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return // -h / --help：flag 包已打印用法
		}
		os.Exit(2)
	}

	if err := run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if opts.showVersion {
		fmt.Fprint(stdout, versionText())
		return nil
	}

	cfg, err := config.Load(opts.configPath)
	if err != nil {
		return err
	}

	if listen := strings.TrimSpace(opts.listen); listen != "" {
		// 命令行覆盖监听地址后重新校验，避免绕过配置约束
		cfg.Listen = listen
		if err := cfg.Validate(); err != nil {
			return fmt.Errorf("-listen 覆盖后的配置非法: %w", err)
		}
	}

	if opts.checkOnly {
		fmt.Fprint(stdout, configSummary(opts.configPath, cfg))
		return nil
	}

	logger := newLogger(cfg.Logging)
	slog.SetDefault(logger)

	// 按配置构造后端实例，并初始化负载均衡器
	backends := make([]*backend.Backend, 0, len(cfg.Backends))
	for _, bc := range cfg.Backends {
		b, err := backend.New(bc.URL, bc.Weight)
		if err != nil {
			return err
		}
		backends = append(backends, b)
	}
	registry := backend.NewRegistry(backends...)

	lb, err := balancer.New(string(cfg.Strategy), registry.All())
	if err != nil {
		return err
	}

	checker := health.New(registry, cfg.HealthCheck, logger)

	handler, err := proxy.New(lb, registry, checker, proxy.Options{
		MaxAttempts:        cfg.Retry.MaxAttempts,
		PerTryTimeout:      cfg.Retry.PerTryTimeout.Duration(),
		RetryOnStatus:      cfg.Retry.RetryOnStatus,
		RetryNonIdempotent: cfg.Retry.RetryNonIdempotent,
	}, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if checker.Enabled() {
		go checker.Run(ctx)
	}

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", cfg.Listen, err)
	}

	srv := &http.Server{
		Handler:           newRouter(handler, logger),
		ReadHeaderTimeout: cfg.Timeouts.ReadHeader.Duration(),
		WriteTimeout:      cfg.Timeouts.Write.Duration(),
		IdleTimeout:       cfg.Timeouts.Idle.Duration(),
	}

	for _, b := range registry.All() {
		logger.Info("后端实例", "url", b.String(), "weight", b.Weight(), "alive", b.Alive())
	}
	logger.Info("反向代理已启动",
		"listen", listener.Addr().String(),
		"strategy", lb.Name(),
		"backends", registry.Len(),
		"healthy", registry.HealthyLen(),
		"health_check", checker.Enabled(),
		"config", opts.configPath,
	)

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("服务异常退出: %w", err)
	case <-ctx.Done():
	}

	logger.Info("收到退出信号，开始优雅关闭")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅关闭失败: %w", err)
	}
	logger.Info("已退出")
	return nil
}

// newRouter 装配 Gin 引擎：把所有路径交给代理处理，并挂上访问日志中间件。
func newRouter(handler *proxy.Handler, logger *slog.Logger) *gin.Engine {
	// 关闭 Gin 自带的调试输出（路由表与模式警告），日志统一走 slog
	gin.SetMode(gin.ReleaseMode)

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(middleware.AccessLog(logger))

	// 保留客户端原始 URL 编码（如 %2F），避免 Gin 二次解码后转发到错误路径
	router.UseRawPath = true
	router.UnescapePathValues = false

	router.Any("/*path", handler.Handle)
	// 兜底：catch-all 未覆盖到的路径（如某些畸形请求）同样交给代理
	router.NoRoute(handler.Handle)
	return router
}

// newLogger 依据配置构造 slog 日志器。
func newLogger(lc config.Logging) *slog.Logger {
	opts := &slog.HandlerOptions{Level: lc.SlogLevel()}
	var handler slog.Handler
	if strings.EqualFold(lc.Format, "text") {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler)
}
