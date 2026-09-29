package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Sader-Lee/reverse-proxy/internal/config"
)

// exampleConfig 是仓库自带的示例配置路径。
func exampleConfig() string {
	return filepath.Join("..", "..", "configs", "config.example.yaml")
}

func TestParseFlagsDefaults(t *testing.T) {
	var stderr bytes.Buffer
	opts, err := parseFlags(nil, &stderr)
	if err != nil {
		t.Fatalf("解析默认参数失败: %v", err)
	}

	if opts.configPath != config.DefaultConfigPath {
		t.Errorf("configPath = %q, 期望 %q", opts.configPath, config.DefaultConfigPath)
	}
	if opts.checkOnly || opts.showVersion {
		t.Errorf("默认不应开启 check/version: %+v", opts)
	}
	if opts.listen != "" {
		t.Errorf("默认不应覆盖监听地址，实际 %q", opts.listen)
	}
}

func TestParseFlagsOverrides(t *testing.T) {
	opts, err := parseFlags([]string{"-config", "/etc/proxy.yaml", "-check", "-listen", ":9090"}, io.Discard)
	if err != nil {
		t.Fatalf("解析参数失败: %v", err)
	}

	if opts.configPath != "/etc/proxy.yaml" {
		t.Errorf("configPath = %q", opts.configPath)
	}
	if !opts.checkOnly {
		t.Error("checkOnly 应为 true")
	}
	if opts.listen != ":9090" {
		t.Errorf("listen = %q, 期望 \":9090\"", opts.listen)
	}
}

func TestParseFlagsUnknownFlag(t *testing.T) {
	var stderr bytes.Buffer
	if _, err := parseFlags([]string{"-nope"}, &stderr); err == nil {
		t.Fatal("未知参数应返回错误")
	}
	if !strings.Contains(stderr.String(), "nope") {
		t.Errorf("错误提示应包含未知参数名，实际: %s", stderr.String())
	}
}

func TestParseFlagsHelp(t *testing.T) {
	var stderr bytes.Buffer
	_, err := parseFlags([]string{"-h"}, &stderr)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v, 期望 flag.ErrHelp", err)
	}
	for _, want := range []string{"-config", "-check", "-version", "-listen"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("用法说明缺少 %s，实际: %s", want, stderr.String())
		}
	}
}

func TestVersionText(t *testing.T) {
	text := versionText()
	if !strings.HasPrefix(text, "reverse-proxy ") {
		t.Errorf("版本信息应以程序名开头，实际: %q", text)
	}
	if !strings.Contains(text, runtime.Version()) {
		t.Errorf("版本信息应包含 Go 版本，实际: %q", text)
	}
	// 在 git 仓库内编译时，Go 会写入 VCS 信息
	if !strings.Contains(text, "commit:") {
		t.Logf("未检测到 commit 信息（可能是非 git 环境构建）: %q", text)
	}
}

func TestConfigSummary(t *testing.T) {
	cfg, err := config.Load(exampleConfig())
	if err != nil {
		t.Fatalf("加载示例配置失败: %v", err)
	}

	summary := configSummary("configs/config.example.yaml", cfg)
	for _, want := range []string{
		"配置校验通过",
		"configs/config.example.yaml",
		":8080",
		"weighted-round-robin",
		"http://127.0.0.1:9001",
		"权重 3",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("摘要缺少 %q，实际:\n%s", want, summary)
		}
	}
}

// captureStdout 替换 stdout 以便断言命令输出。
func captureStdout(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	original := stdout
	stdout = &buf
	t.Cleanup(func() { stdout = original })
	return &buf
}

func TestRunCheckOnly(t *testing.T) {
	buf := captureStdout(t)

	if err := run(options{configPath: exampleConfig(), checkOnly: true}); err != nil {
		t.Fatalf("校验示例配置失败: %v", err)
	}
	if !strings.Contains(buf.String(), "配置校验通过") {
		t.Errorf("应输出校验结果，实际: %s", buf.String())
	}
}

func TestRunShowVersion(t *testing.T) {
	buf := captureStdout(t)

	// -version 不应读取配置文件，因此给一个不存在的路径也能正常工作
	err := run(options{configPath: filepath.Join(t.TempDir(), "not-exist.yaml"), showVersion: true})
	if err != nil {
		t.Fatalf("打印版本失败: %v", err)
	}
	if !strings.Contains(buf.String(), "reverse-proxy") {
		t.Errorf("应输出版本信息，实际: %s", buf.String())
	}
}

func TestRunRejectsInvalidConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("listen: \"\"\nstrategy: \"unknown\"\n"), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}

	if err := run(options{configPath: path, checkOnly: true}); err == nil {
		t.Error("非法配置应返回错误")
	}
}

func TestRunListenOverride(t *testing.T) {
	buf := captureStdout(t)

	opts := options{configPath: exampleConfig(), checkOnly: true, listen: ":9090"}
	if err := run(opts); err != nil {
		t.Fatalf("覆盖监听地址失败: %v", err)
	}
	if !strings.Contains(buf.String(), ":9090") {
		t.Errorf("摘要应体现覆盖后的监听地址，实际: %s", buf.String())
	}
}

func TestRunIgnoresBlankListenOverride(t *testing.T) {
	buf := captureStdout(t)

	// 空白值视为"未提供"，沿用配置文件里的监听地址
	opts := options{configPath: exampleConfig(), checkOnly: true, listen: "   "}
	if err := run(opts); err != nil {
		t.Fatalf("空白覆盖值不应报错: %v", err)
	}
	if strings.Contains(buf.String(), "9090") {
		t.Errorf("空白覆盖值不应生效，实际: %s", buf.String())
	}
	if !strings.Contains(buf.String(), ":8080") {
		t.Errorf("应沿用配置中的监听地址，实际: %s", buf.String())
	}
}
