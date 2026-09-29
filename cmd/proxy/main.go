// Command proxy 是反向代理与负载均衡器的入口。
//
// 阶段 1：基于 Gin + httputil.ReverseProxy 把请求转发到配置中的后端实例，
// 并记录访问日志；多后端负载均衡、健康检查与超时重试在后续阶段接入。
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

	"github.com/Sader-Lee/reverse-proxy/internal/config"
	"github.com/Sader-Lee/reverse-proxy/internal/middleware"
	"github.com/Sader-Lee/reverse-proxy/internal/proxy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", config.DefaultConfigPath, "配置文件路径（YAML）")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := newLogger(cfg.Logging)
	slog.SetDefault(logger)

	// 阶段 1 只转发到配置中的第一个后端，多实例负载均衡在阶段 2 接入。
	target := cfg.Backends[0].URL
	if len(cfg.Backends) > 1 {
		logger.Warn("当前阶段仅使用第一个后端，其余实例暂未参与转发",
			"using", target, "configured", len(cfg.Backends))
	}

	handler, err := proxy.New(target, logger)
	if err != nil {
		return err
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

	logger.Info("反向代理已启动",
		"listen", listener.Addr().String(),
		"target", handler.Target(),
		"strategy", string(cfg.Strategy),
		"health_check", cfg.HealthCheck.Enabled,
		"config", *configPath,
	)

	serveErr := make(chan error, 1)
	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
