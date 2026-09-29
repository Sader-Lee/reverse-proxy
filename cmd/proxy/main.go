// Command proxy 是反向代理与负载均衡器的入口。
//
// 阶段 0 仅完成配置加载与日志初始化；请求转发能力将在后续阶段接入。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/Sader-Lee/reverse-proxy/internal/config"
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
	logger.Info("配置加载成功",
		"path", *configPath,
		"listen", cfg.Listen,
		"strategy", string(cfg.Strategy),
		"backends", len(cfg.Backends),
	)

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化生效配置失败: %w", err)
	}
	fmt.Println(string(out))
	return nil
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
