// Package balancer 提供后端实例的负载均衡策略。
//
// 所有实现都必须并发安全，且在没有存活实例时返回 nil。
package balancer

import (
	"fmt"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
)

// 策略名，与配置文件中 strategy 字段的取值一致。
const (
	NameRoundRobin         = "round-robin"
	NameRandom             = "random"
	NameWeightedRoundRobin = "weighted-round-robin"
)

// Balancer 从一组后端实例中挑选下一次转发的目标。
type Balancer interface {
	// Next 返回下一个可用实例，exclude 中的实例会被跳过（用于失败后换实例重试）；
	// 没有可用实例时返回 nil。
	Next(exclude ...*backend.Backend) *backend.Backend
	// Name 返回策略名。
	Name() string
}

// New 按策略名构造均衡器。
func New(strategy string, backends []*backend.Backend) (Balancer, error) {
	if len(backends) == 0 {
		return nil, fmt.Errorf("构造负载均衡器至少需要一个后端实例")
	}

	switch strategy {
	case NameRoundRobin:
		return NewRoundRobin(backends), nil
	case NameRandom:
		return NewRandom(backends), nil
	case NameWeightedRoundRobin:
		return NewWeightedRoundRobin(backends), nil
	default:
		return nil, fmt.Errorf("不支持的负载均衡策略 %q（可选：%s / %s / %s）",
			strategy, NameRoundRobin, NameRandom, NameWeightedRoundRobin)
	}
}

// excluded 判断实例是否在排除列表中。
// 排除列表很短（不超过重试次数），线性扫描足够。
func excluded(b *backend.Backend, list []*backend.Backend) bool {
	for _, e := range list {
		if e == b {
			return true
		}
	}
	return false
}

// availableCount 统计既存活又未被排除的实例数量。
func availableCount(backends, exclude []*backend.Backend) int {
	count := 0
	for _, b := range backends {
		if b.Alive() && !excluded(b, exclude) {
			count++
		}
	}
	return count
}

// pickNthAvailable 返回第 n 个可用实例（n 从 0 开始），不足时返回 nil。
//
// 采用"先统计再定位"的两趟遍历，好处是按可用实例数量取模：
// 某个实例下线或被排除时，剩余实例之间依然严格轮流/均分，且整个过程零分配。
func pickNthAvailable(backends []*backend.Backend, n int, exclude []*backend.Backend) *backend.Backend {
	if n < 0 {
		return nil
	}
	for _, b := range backends {
		if !b.Alive() || excluded(b, exclude) {
			continue
		}
		if n == 0 {
			return b
		}
		n--
	}
	return nil
}
