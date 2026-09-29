// Package backend 定义后端实例模型与实例注册表。
//
// 实例的存活状态与统计数据会被多个 goroutine 并发读写（转发、健康检查、日志），
// 因此全部使用原子类型，不需要额外的锁。
package backend

import (
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
)

// Backend 表示一个后端实例。
type Backend struct {
	rawURL string
	url    *url.URL
	weight int

	alive atomic.Bool

	requests atomic.Int64
	failures atomic.Int64
}

// New 构造一个后端实例。weight 小于等于 0 时按 1 处理。
// 新实例默认视为存活，后续状态由健康检查负责维护。
func New(rawURL string, weight int) (*Backend, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(rawURL), "/")
	if trimmed == "" {
		return nil, fmt.Errorf("后端地址不能为空")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("解析后端地址 %q 失败: %w", rawURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("后端地址 %q 的 scheme 必须是 http 或 https", rawURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("后端地址 %q 缺少主机名", rawURL)
	}
	if weight <= 0 {
		weight = 1
	}

	b := &Backend{rawURL: trimmed, url: u, weight: weight}
	b.alive.Store(true)
	return b, nil
}

// URL 返回后端地址的副本，避免调用方修改共享状态。
func (b *Backend) URL() *url.URL {
	clone := *b.url
	return &clone
}

// String 返回后端地址原文。
func (b *Backend) String() string { return b.rawURL }

// Weight 返回后端权重，最小为 1。
func (b *Backend) Weight() int { return b.weight }

// Alive 返回后端当前是否存活。
func (b *Backend) Alive() bool { return b.alive.Load() }

// SetAlive 设置存活状态，返回状态是否发生变化。
func (b *Backend) SetAlive(alive bool) bool {
	return b.alive.Swap(alive) != alive
}

// RecordRequest 记录一次转发。
func (b *Backend) RecordRequest() { b.requests.Add(1) }

// RecordFailure 记录一次转发失败。
func (b *Backend) RecordFailure() { b.failures.Add(1) }

// Stats 是后端实例的累计统计。
type Stats struct {
	Requests int64
	Failures int64
}

// Stats 返回后端实例的累计统计。
func (b *Backend) Stats() Stats {
	return Stats{
		Requests: b.requests.Load(),
		Failures: b.failures.Load(),
	}
}

// Registry 保存全部后端实例，供负载均衡与健康检查共享。
// 实例集合在构造后不再变化，因此无需加锁。
type Registry struct {
	backends []*Backend
}

// NewRegistry 用给定实例构造注册表。
func NewRegistry(backends ...*Backend) *Registry {
	return &Registry{backends: backends}
}

// All 返回全部实例的快照，调用方可以自由修改返回值。
func (r *Registry) All() []*Backend {
	snapshot := make([]*Backend, len(r.backends))
	copy(snapshot, r.backends)
	return snapshot
}

// Healthy 返回当前存活实例的快照。
func (r *Registry) Healthy() []*Backend {
	healthy := make([]*Backend, 0, len(r.backends))
	for _, b := range r.backends {
		if b.Alive() {
			healthy = append(healthy, b)
		}
	}
	return healthy
}

// Len 返回实例总数。
func (r *Registry) Len() int { return len(r.backends) }

// HealthyLen 返回当前存活实例数量。
func (r *Registry) HealthyLen() int {
	count := 0
	for _, b := range r.backends {
		if b.Alive() {
			count++
		}
	}
	return count
}
