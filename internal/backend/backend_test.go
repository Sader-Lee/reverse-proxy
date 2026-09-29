package backend

import (
	"strings"
	"sync"
	"testing"
)

func TestNewNormalizesInput(t *testing.T) {
	tests := []struct {
		name       string
		rawURL     string
		weight     int
		wantURL    string
		wantHost   string
		wantWeight int
	}{
		{name: "去尾部斜杠与空格", rawURL: "  http://127.0.0.1:9001/  ", weight: 3, wantURL: "http://127.0.0.1:9001", wantHost: "127.0.0.1:9001", wantWeight: 3},
		{name: "权重为 0 归一为 1", rawURL: "http://127.0.0.1:9001", weight: 0, wantURL: "http://127.0.0.1:9001", wantHost: "127.0.0.1:9001", wantWeight: 1},
		{name: "负权重归一为 1", rawURL: "http://127.0.0.1:9001", weight: -3, wantURL: "http://127.0.0.1:9001", wantHost: "127.0.0.1:9001", wantWeight: 1},
		{name: "保留基础路径", rawURL: "http://127.0.0.1:9001/base", weight: 1, wantURL: "http://127.0.0.1:9001/base", wantHost: "127.0.0.1:9001", wantWeight: 1},
		{name: "支持 https", rawURL: "https://example.com", weight: 1, wantURL: "https://example.com", wantHost: "example.com", wantWeight: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := New(tc.rawURL, tc.weight)
			if err != nil {
				t.Fatalf("构造后端失败: %v", err)
			}
			if b.String() != tc.wantURL {
				t.Errorf("URL = %q, 期望 %q", b.String(), tc.wantURL)
			}
			if b.Weight() != tc.wantWeight {
				t.Errorf("Weight = %d, 期望 %d", b.Weight(), tc.wantWeight)
			}
			if !b.Alive() {
				t.Error("新建实例应默认为存活状态")
			}
			if got := b.URL().Host; got != tc.wantHost {
				t.Errorf("URL().Host = %q, 期望 %q", got, tc.wantHost)
			}
		})
	}
}

func TestNewRejectsInvalidURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantErr string
	}{
		{name: "空地址", rawURL: "  ", wantErr: "不能为空"},
		{name: "非法 scheme", rawURL: "tcp://127.0.0.1:9001", wantErr: "scheme"},
		{name: "缺少主机名", rawURL: "http:///api", wantErr: "缺少主机名"},
		{name: "非法转义", rawURL: "http://127.0.0.1:9001/%zz", wantErr: "解析后端地址"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(tc.rawURL, 1)
			if err == nil {
				t.Fatalf("rawURL=%q 期望报错，实际通过", tc.rawURL)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误信息未包含 %q，实际: %v", tc.wantErr, err)
			}
		})
	}
}

func TestURLReturnsCopy(t *testing.T) {
	b, err := New("http://127.0.0.1:9001/api", 1)
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}

	u := b.URL()
	u.Host = "mutated:1234"
	u.Path = "/mutated"

	if got := b.URL().Host; got != "127.0.0.1:9001" {
		t.Errorf("外部修改已影响内部状态: Host = %q", got)
	}
	if got := b.URL().Path; got != "/api" {
		t.Errorf("外部修改已影响内部状态: Path = %q", got)
	}
}

func TestSetAliveReportsChange(t *testing.T) {
	b, err := New("http://127.0.0.1:9001", 1)
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}

	if !b.SetAlive(false) {
		t.Error("存活 -> 下线 应报告状态变化")
	}
	if b.Alive() {
		t.Error("SetAlive(false) 后应为不存活")
	}
	if b.SetAlive(false) {
		t.Error("重复设置同一状态不应报告变化")
	}
	if !b.SetAlive(true) {
		t.Error("下线 -> 存活 应报告状态变化")
	}
	if !b.Alive() {
		t.Error("SetAlive(true) 后应为存活")
	}
}

func TestStats(t *testing.T) {
	b, err := New("http://127.0.0.1:9001", 1)
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}

	for i := 0; i < 5; i++ {
		b.RecordRequest()
	}
	b.RecordFailure()
	b.RecordFailure()

	stats := b.Stats()
	if stats.Requests != 5 {
		t.Errorf("Requests = %d, 期望 5", stats.Requests)
	}
	if stats.Failures != 2 {
		t.Errorf("Failures = %d, 期望 2", stats.Failures)
	}
}

func TestRegistryCountsAndSnapshot(t *testing.T) {
	first, _ := New("http://127.0.0.1:9001", 1)
	second, _ := New("http://127.0.0.1:9002", 2)
	third, _ := New("http://127.0.0.1:9003", 1)
	registry := NewRegistry(first, second, third)

	if registry.Len() != 3 {
		t.Errorf("Len = %d, 期望 3", registry.Len())
	}
	if registry.HealthyLen() != 3 {
		t.Errorf("HealthyLen = %d, 期望 3", registry.HealthyLen())
	}

	second.SetAlive(false)
	if registry.HealthyLen() != 2 {
		t.Errorf("下线一个后 HealthyLen = %d, 期望 2", registry.HealthyLen())
	}
	healthy := registry.Healthy()
	if len(healthy) != 2 {
		t.Fatalf("Healthy 长度 = %d, 期望 2", len(healthy))
	}
	for _, b := range healthy {
		if b == second {
			t.Error("Healthy 不应包含已下线实例")
		}
	}

	// 快照可被调用方自由修改，不影响注册表内部状态
	snapshot := registry.All()
	snapshot[0] = nil
	if registry.All()[0] == nil {
		t.Error("修改快照影响了注册表内部状态")
	}

	all := registry.All()
	if all[0] != first || all[2] != third {
		t.Error("All() 应保持构造时的顺序")
	}
}

func TestConcurrentAccess(t *testing.T) {
	b, err := New("http://127.0.0.1:9001", 2)
	if err != nil {
		t.Fatalf("构造后端失败: %v", err)
	}
	registry := NewRegistry(b)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				b.SetAlive(j%2 == 0)
				_ = b.Alive()
				b.RecordRequest()
				b.RecordFailure()
				_ = b.Stats()
				_ = registry.Healthy()
				_ = registry.HealthyLen()
			}
		}()
	}
	wg.Wait()

	if got := b.Stats().Requests; got != 8*500 {
		t.Errorf("Requests = %d, 期望 %d", got, 8*500)
	}
}
