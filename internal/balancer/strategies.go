package balancer

import (
	"math/rand/v2"
	"sync"
	"sync/atomic"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
)

// roundRobin 轮询：在存活实例之间依次轮流。
type roundRobin struct {
	backends []*backend.Backend
	counter  atomic.Uint64
}

// NewRoundRobin 构造轮询均衡器。
func NewRoundRobin(backends []*backend.Backend) Balancer {
	return &roundRobin{backends: backends}
}

// Name 实现 Balancer。
func (r *roundRobin) Name() string { return NameRoundRobin }

// Next 实现 Balancer。
func (r *roundRobin) Next(exclude ...*backend.Backend) *backend.Backend {
	available := availableCount(r.backends, exclude)
	if available == 0 {
		return nil
	}
	return pickNthAvailable(r.backends, int(r.counter.Add(1)-1)%available, exclude)
}

// indexPicker 返回 [0, n) 区间内的下标，便于测试注入确定性实现。
type indexPicker func(n int) int

// random 随机：在存活实例中随机挑选。
type random struct {
	backends []*backend.Backend
	pick     indexPicker
}

// NewRandom 构造随机均衡器。
// 使用 math/rand/v2 的包级函数，它并发安全且无需手动播种。
func NewRandom(backends []*backend.Backend) Balancer {
	return newRandom(backends, rand.IntN)
}

func newRandom(backends []*backend.Backend, pick indexPicker) Balancer {
	return &random{backends: backends, pick: pick}
}

// Name 实现 Balancer。
func (r *random) Name() string { return NameRandom }

// Next 实现 Balancer。
func (r *random) Next(exclude ...*backend.Backend) *backend.Backend {
	available := availableCount(r.backends, exclude)
	if available == 0 {
		return nil
	}
	return pickNthAvailable(r.backends, r.pick(available), exclude)
}

// weightedRoundRobin 平滑加权轮询（nginx 同款算法）。
//
// 每轮为存活实例累加自身权重，选出当前权重最大者，并让它减去权重总和。
// 相比简单加权轮询（连续打同一实例多次），它可以打散请求，避免瞬时集中。
type weightedRoundRobin struct {
	mu       sync.Mutex
	backends []*backend.Backend
	current  []int
}

// NewWeightedRoundRobin 构造平滑加权轮询均衡器。
func NewWeightedRoundRobin(backends []*backend.Backend) Balancer {
	return &weightedRoundRobin{
		backends: backends,
		current:  make([]int, len(backends)),
	}
}

// Name 实现 Balancer。
func (w *weightedRoundRobin) Name() string { return NameWeightedRoundRobin }

// Next 实现 Balancer。
func (w *weightedRoundRobin) Next(exclude ...*backend.Backend) *backend.Backend {
	w.mu.Lock()
	defer w.mu.Unlock()

	best := -1
	total := 0
	for i, b := range w.backends {
		if !b.Alive() {
			w.current[i] = 0 // 下线实例不参与竞争，恢复后从 0 重新累计
			continue
		}
		if excluded(b, exclude) {
			// 临时排除（本次重试已试过）：不累加也不清零，保留平滑轮询的进度
			continue
		}
		w.current[i] += b.Weight()
		total += b.Weight()
		if best == -1 || w.current[i] > w.current[best] {
			best = i
		}
	}
	if best == -1 || total == 0 {
		return nil
	}

	w.current[best] -= total
	return w.backends[best]
}
