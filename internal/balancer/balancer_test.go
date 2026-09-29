package balancer

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
)

// testBackend 构造一个测试用后端实例。
func testBackend(t *testing.T, rawURL string, weight int) *backend.Backend {
	t.Helper()
	b, err := backend.New(rawURL, weight)
	if err != nil {
		t.Fatalf("构造后端 %s 失败: %v", rawURL, err)
	}
	return b
}

// testBackends 构造 n 个权重为 1 的后端实例。
func testBackends(t *testing.T, n int) []*backend.Backend {
	t.Helper()
	list := make([]*backend.Backend, 0, n)
	for i := 0; i < n; i++ {
		list = append(list, testBackend(t, fmt.Sprintf("http://127.0.0.1:%d", 9001+i), 1))
	}
	return list
}

// distribution 连续调用 Next 若干次，返回各实例被选中的次数。
func distribution(t *testing.T, lb Balancer, times int) map[string]int {
	t.Helper()
	counts := make(map[string]int)
	for i := 0; i < times; i++ {
		b := lb.Next()
		if b == nil {
			t.Fatalf("第 %d 次调用返回 nil", i)
		}
		counts[b.String()]++
	}
	return counts
}

func TestNewRejectsUnknownStrategyAndEmptyBackends(t *testing.T) {
	if _, err := New("least-conn", testBackends(t, 2)); err == nil {
		t.Error("未知策略应报错")
	}
	if _, err := New(NameRoundRobin, nil); err == nil {
		t.Error("后端列表为空时应报错")
	}
}

func TestNewReturnsExpectedStrategies(t *testing.T) {
	backends := testBackends(t, 2)
	cases := map[string]string{
		NameRoundRobin:         NameRoundRobin,
		NameRandom:             NameRandom,
		NameWeightedRoundRobin: NameWeightedRoundRobin,
	}
	for strategy, wantName := range cases {
		lb, err := New(strategy, backends)
		if err != nil {
			t.Fatalf("构造 %s 失败: %v", strategy, err)
		}
		if lb.Name() != wantName {
			t.Errorf("Name = %q, 期望 %q", lb.Name(), wantName)
		}
	}
}

func TestRoundRobinCyclesInOrder(t *testing.T) {
	backends := testBackends(t, 3)
	lb := NewRoundRobin(backends)

	for round := 0; round < 4; round++ {
		for i, want := range backends {
			got := lb.Next()
			if got == nil {
				t.Fatalf("第 %d 轮第 %d 次返回 nil", round, i)
			}
			if got != want {
				t.Errorf("第 %d 轮第 %d 次 = %s, 期望 %s", round, i, got.String(), want.String())
			}
		}
	}
}

func TestRoundRobinDistributesEvenlyWhenSomeBackendsDown(t *testing.T) {
	backends := testBackends(t, 3)
	lb := NewRoundRobin(backends)

	backends[0].SetAlive(false) // 下线第一个，余下两个应严格 50/50

	counts := distribution(t, lb, 100)
	if counts[backends[0].String()] != 0 {
		t.Errorf("不应选中已下线实例，实际 %d 次", counts[backends[0].String()])
	}
	if counts[backends[1].String()] != 50 || counts[backends[2].String()] != 50 {
		t.Errorf("存活实例之间未均分: %v", counts)
	}
}

func TestRoundRobinReturnsNilWhenAllDown(t *testing.T) {
	backends := testBackends(t, 2)
	lb := NewRoundRobin(backends)

	for _, b := range backends {
		b.SetAlive(false)
	}
	if got := lb.Next(); got != nil {
		t.Errorf("全部下线时应返回 nil，实际 %v", got)
	}

	// 恢复后应重新可用
	backends[1].SetAlive(true)
	if got := lb.Next(); got != backends[1] {
		t.Errorf("恢复后应重新选中该实例，实际 %v", got)
	}
}

func TestRandomUsesInjectedPicker(t *testing.T) {
	backends := testBackends(t, 3)

	var calls int
	lb := newRandom(backends, func(n int) int {
		calls++
		if n != 3 {
			t.Errorf("picker 收到的存活实例数 = %d, 期望 3", n)
		}
		return 2
	})

	if got := lb.Next(); got != backends[2] {
		t.Errorf("Next = %v, 期望 %v", got, backends[2])
	}
	if calls != 1 {
		t.Errorf("picker 调用次数 = %d, 期望 1", calls)
	}
}

func TestRandomPicksOnlyAliveAndCoversAll(t *testing.T) {
	backends := testBackends(t, 3)
	lb := NewRandom(backends)

	backends[1].SetAlive(false)

	const times = 3000
	counts := distribution(t, lb, times)
	if counts[backends[1].String()] != 0 {
		t.Errorf("不应选中已下线实例，实际 %d 次", counts[backends[1].String()])
	}
	for _, b := range []*backend.Backend{backends[0], backends[2]} {
		got := counts[b.String()]
		// 随机分布允许波动，只做宽松区间校验（期望 1500，允许 ±10%）
		if got < 1350 || got > 1650 {
			t.Errorf("%s 被选中 %d 次，偏离期望值 1500 过多", b.String(), got)
		}
	}
}

func TestWeightedRoundRobinSmoothSequence(t *testing.T) {
	backends := []*backend.Backend{
		testBackend(t, "http://127.0.0.1:9001", 3),
		testBackend(t, "http://127.0.0.1:9002", 1),
	}
	lb := NewWeightedRoundRobin(backends)

	// 权重 3:1 的平滑序列为 A A B A 循环，而不是 A A A B
	want := []*backend.Backend{
		backends[0], backends[0], backends[1], backends[0],
		backends[0], backends[0], backends[1], backends[0],
	}
	for i, expected := range want {
		got := lb.Next()
		if got != expected {
			t.Errorf("第 %d 次 = %s, 期望 %s", i, got.String(), expected.String())
		}
	}
}

func TestWeightedRoundRobinHonorsWeights(t *testing.T) {
	backends := []*backend.Backend{
		testBackend(t, "http://127.0.0.1:9001", 5),
		testBackend(t, "http://127.0.0.1:9002", 1),
		testBackend(t, "http://127.0.0.1:9003", 1),
	}
	lb := NewWeightedRoundRobin(backends)

	counts := distribution(t, lb, 70) // 5:1:1，每 7 次一轮，共 10 轮
	wants := map[string]int{
		backends[0].String(): 50,
		backends[1].String(): 10,
		backends[2].String(): 10,
	}
	for url, want := range wants {
		if counts[url] != want {
			t.Errorf("%s 被选中 %d 次, 期望 %d 次（完整周期内应严格按权重分配）", url, counts[url], want)
		}
	}
}

func TestWeightedRoundRobinRedistributesWhenBackendDown(t *testing.T) {
	t.Run("下线高权重实例后流量全给存活实例", func(t *testing.T) {
		backends := []*backend.Backend{
			testBackend(t, "http://127.0.0.1:9001", 3),
			testBackend(t, "http://127.0.0.1:9002", 1),
		}
		lb := NewWeightedRoundRobin(backends)
		backends[0].SetAlive(false)

		counts := distribution(t, lb, 20)
		if counts[backends[1].String()] != 20 {
			t.Errorf("流量应全部落到存活实例，实际分布: %v", counts)
		}
	})

	t.Run("等权重实例下线后剩余实例均分", func(t *testing.T) {
		backends := testBackends(t, 3)
		lb := NewWeightedRoundRobin(backends)
		backends[1].SetAlive(false)

		counts := distribution(t, lb, 100)
		if counts[backends[0].String()] != 50 || counts[backends[2].String()] != 50 {
			t.Errorf("存活实例之间未均分: %v", counts)
		}
	})
}

func TestWeightedRoundRobinReturnsNilWhenAllDown(t *testing.T) {
	backends := testBackends(t, 2)
	lb := NewWeightedRoundRobin(backends)

	for _, b := range backends {
		b.SetAlive(false)
	}
	if got := lb.Next(); got != nil {
		t.Errorf("全部下线时应返回 nil，实际 %v", got)
	}
}

func TestAllStrategiesAreConcurrencySafe(t *testing.T) {
	for _, strategy := range []string{NameRoundRobin, NameRandom, NameWeightedRoundRobin} {
		t.Run(strategy, func(t *testing.T) {
			backends := testBackends(t, 4)
			lb, err := New(strategy, backends)
			if err != nil {
				t.Fatalf("构造均衡器失败: %v", err)
			}

			const goroutines, perGoroutine = 16, 300

			var (
				wg    sync.WaitGroup
				total atomic.Int64
			)
			for i := 0; i < goroutines; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := 0; j < perGoroutine; j++ {
						if lb.Next() == nil {
							t.Error("存在存活实例时不应返回 nil")
							return
						}
						total.Add(1)
					}
				}()
			}
			wg.Wait()

			if got := total.Load(); got != goroutines*perGoroutine {
				t.Errorf("成功选中 %d 次, 期望 %d", got, goroutines*perGoroutine)
			}
		})
	}
}

func TestAllStrategiesTolerateConcurrentStateChanges(t *testing.T) {
	for _, strategy := range []string{NameRoundRobin, NameRandom, NameWeightedRoundRobin} {
		t.Run(strategy, func(t *testing.T) {
			backends := testBackends(t, 3)
			lb, err := New(strategy, backends)
			if err != nil {
				t.Fatalf("构造均衡器失败: %v", err)
			}

			valid := make(map[*backend.Backend]bool, len(backends))
			for _, b := range backends {
				valid[b] = true
			}

			done := make(chan struct{})
			flipperDone := make(chan struct{})

			go func() { // 持续切换存活状态，模拟健康检查与转发流量并发
				defer close(flipperDone)
				for {
					select {
					case <-done:
						return
					default:
					}
					for _, b := range backends {
						b.SetAlive(!b.Alive())
					}
				}
			}()

			var readers sync.WaitGroup
			for i := 0; i < 8; i++ {
				readers.Add(1)
				go func() {
					defer readers.Done()
					for j := 0; j < 500; j++ {
						// 注意：返回的实例可能在被返回的一瞬间被健康检查刷成下线，
						// 这是并发下正常的窗口，不能断言"返回值必须仍存活"，
						// 这里只校验不会返回未知实例、不会 panic（配合 -race 使用）。
						if b := lb.Next(); b != nil && !valid[b] {
							t.Error("返回了不属于该均衡器的实例")
							return
						}
					}
				}()
			}

			readers.Wait()
			close(done)
			<-flipperDone
		})
	}
}
