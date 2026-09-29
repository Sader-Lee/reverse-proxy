package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/Sader-Lee/reverse-proxy/internal/config"
)

// 版本信息，可在编译期通过 -ldflags 注入（Makefile 已接好）：
//
//	go build -ldflags "-X main.version=v1.0.0 -X main.commit=abc1234 -X main.builtAt=..."
//
// 未注入时会回落到 Go 自动写入的 git 信息。
var (
	version = "dev"
	commit  = ""
	builtAt = ""
)

// stdout 便于测试替换，正常运行时始终是 os.Stdout。
var stdout io.Writer = os.Stdout

// options 是命令行参数。
//
// 只提供配置文件无法表达的能力（定位配置、动作型参数、部署时覆盖），
// 不把 YAML 里的字段再抄一遍，避免配置出现两份来源。
type options struct {
	configPath  string
	checkOnly   bool
	showVersion bool
	listen      string
}

// parseFlags 解析命令行参数。解析失败时 flag 包已把用法写入 stderr。
func parseFlags(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var opts options
	fs.StringVar(&opts.configPath, "config", config.DefaultConfigPath, "配置文件路径（YAML）")
	fs.BoolVar(&opts.checkOnly, "check", false, "只校验配置文件并退出，不启动服务")
	fs.BoolVar(&opts.showVersion, "version", false, "打印版本信息并退出")
	fs.StringVar(&opts.listen, "listen", "", "覆盖配置文件中的监听地址，例如 :9090（留空则不覆盖）")

	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	return opts, nil
}

// versionText 返回版本信息文本。
func versionText() string {
	info, _ := debug.ReadBuildInfo()

	name := version
	if name == "dev" && info != nil && info.Main.Version != "" && info.Main.Version != "(devel)" {
		name = info.Main.Version
	}

	revision, buildTime, modified := vcsInfo(info)
	if commit == "" {
		commit = revision
	}
	if builtAt == "" {
		builtAt = buildTime
	}

	var b strings.Builder
	fmt.Fprintf(&b, "reverse-proxy %s\n", name)
	if commit != "" {
		fmt.Fprintf(&b, "  commit:     %s", commit)
		if modified == "true" {
			b.WriteString("（工作区有未提交改动）")
		}
		b.WriteString("\n")
	}
	if builtAt != "" {
		fmt.Fprintf(&b, "  build time: %s\n", builtAt)
	}
	fmt.Fprintf(&b, "  go:         %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return b.String()
}

// vcsInfo 从 Go 写入的构建信息里读取 git 版本信息。
func vcsInfo(info *debug.BuildInfo) (revision, buildTime, modified string) {
	if info == nil {
		return "", "", ""
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			buildTime = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	return revision, buildTime, modified
}

// configSummary 返回生效配置的摘要，供 -check 输出。
func configSummary(path string, cfg *config.Config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "配置校验通过: %s\n", path)
	fmt.Fprintf(&b, "  listen:       %s\n", cfg.Listen)
	fmt.Fprintf(&b, "  strategy:     %s\n", cfg.Strategy)
	fmt.Fprintf(&b, "  health_check: %v（周期 %s）\n", cfg.HealthCheck.Enabled, cfg.HealthCheck.Interval.Duration())
	fmt.Fprintf(&b, "  retry:        最多 %d 次尝试，单次超时 %s\n",
		cfg.Retry.MaxAttempts, cfg.Retry.PerTryTimeout.Duration())
	fmt.Fprintf(&b, "  backends:     %d 个\n", len(cfg.Backends))
	for _, backend := range cfg.Backends {
		fmt.Fprintf(&b, "    - %s（权重 %d）\n", backend.URL, backend.Weight)
	}
	return b.String()
}
