package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// dialError 构造一个"建立连接失败"的错误。
func dialError() error {
	return &net.OpError{Op: "dial", Err: errors.New("connection refused")}
}

func TestOptionsNormalize(t *testing.T) {
	tests := []struct {
		name             string
		in               Options
		wantMaxAttempts  int
		wantPerTryTimout time.Duration
	}{
		{
			name:            "零值按只尝试一次处理",
			in:              Options{},
			wantMaxAttempts: 1,
		},
		{
			name:            "负数尝试次数按一次处理",
			in:              Options{MaxAttempts: -3},
			wantMaxAttempts: 1,
		},
		{
			name:             "负超时按不限制处理",
			in:               Options{MaxAttempts: 2, PerTryTimeout: -time.Second},
			wantMaxAttempts:  2,
			wantPerTryTimout: 0,
		},
		{
			name:             "合法值原样保留",
			in:               Options{MaxAttempts: 5, PerTryTimeout: 2 * time.Second},
			wantMaxAttempts:  5,
			wantPerTryTimout: 2 * time.Second,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.normalize()
			if got.MaxAttempts != tc.wantMaxAttempts {
				t.Errorf("MaxAttempts = %d, 期望 %d", got.MaxAttempts, tc.wantMaxAttempts)
			}
			if got.PerTryTimeout != tc.wantPerTryTimout {
				t.Errorf("PerTryTimeout = %v, 期望 %v", got.PerTryTimeout, tc.wantPerTryTimout)
			}
		})
	}
}

func TestOptionsRetryStatusSet(t *testing.T) {
	if got := (Options{}).retryStatusSet(); got != nil {
		t.Errorf("未配置状态码时应返回 nil，实际 %v", got)
	}

	set := Options{RetryOnStatus: []int{502, 503, 504}}.retryStatusSet()
	if len(set) != 3 || !set[502] || !set[503] || !set[504] {
		t.Errorf("状态码集合 = %v, 期望包含 502/503/504", set)
	}
	if set[500] {
		t.Error("未配置的状态码不应在集合中")
	}
}

func TestIsDialError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "建立连接失败", err: dialError(), want: true},
		{name: "包装后的建立连接失败", err: fmt.Errorf("转发失败: %w", dialError()), want: true},
		{name: "读写阶段的错误不算", err: &net.OpError{Op: "read", Err: errors.New("reset")}, want: false},
		{name: "超时不算", err: context.DeadlineExceeded, want: false},
		{name: "普通错误不算", err: errors.New("boom"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDialError(tc.err); got != tc.want {
				t.Errorf("isDialError(%v) = %v, 期望 %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsIdempotentPerMethod(t *testing.T) {
	// 幂等性判定是重试策略的基础，逐个方法校验
	for _, method := range []string{"GET", "HEAD", "PUT", "DELETE", "OPTIONS", "TRACE"} {
		if !isIdempotent(method) {
			t.Errorf("%s 应被视为幂等", method)
		}
	}
	for _, method := range []string{"POST", "PATCH", "CUSTOM"} {
		if isIdempotent(method) {
			t.Errorf("%s 不应被视为幂等", method)
		}
	}
}

func TestCanRetryTransport(t *testing.T) {
	tests := []struct {
		name  string
		state attemptState
		err   error
		want  bool
	}{
		{
			name:  "幂等方法遇到超时可以重试",
			state: attemptState{replayable: true, idempotent: true},
			err:   context.DeadlineExceeded,
			want:  true,
		},
		{
			name:  "非幂等方法遇到 dial 失败可以重试",
			state: attemptState{replayable: true},
			err:   dialError(),
			want:  true,
		},
		{
			name:  "非幂等方法遇到超时不重试",
			state: attemptState{replayable: true},
			err:   context.DeadlineExceeded,
			want:  false,
		},
		{
			name:  "开启 retry_non_idempotent 后非幂等方法超时可重试",
			state: attemptState{replayable: true, allowAnyRetry: true},
			err:   context.DeadlineExceeded,
			want:  true,
		},
		{
			name:  "请求体不可重放时不重试",
			state: attemptState{replayable: false, idempotent: true},
			err:   dialError(),
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.canRetryTransport(tc.err); got != tc.want {
				t.Errorf("canRetryTransport = %v, 期望 %v", got, tc.want)
			}
		})
	}
}

func TestShouldRetryStatus(t *testing.T) {
	retryable := map[int]bool{502: true, 503: true, 504: true}
	base := func() attemptState {
		return attemptState{
			attempt:     1,
			maxAttempts: 3,
			replayable:  true,
			idempotent:  true,
			retryStatus: retryable,
		}
	}

	t.Run("命中配置的状态码", func(t *testing.T) {
		st := base()
		if !st.shouldRetryStatus(503) {
			t.Error("503 在重试名单中，应触发重试")
		}
		if st.shouldRetryStatus(500) {
			t.Error("500 不在重试名单中，不应触发重试")
		}
	})

	t.Run("没有剩余尝试次数", func(t *testing.T) {
		st := base()
		st.attempt = st.maxAttempts
		if st.shouldRetryStatus(503) {
			t.Error("已是最后一次尝试，不应再触发重试")
		}
	})

	t.Run("非幂等方法默认不按状态码重试", func(t *testing.T) {
		st := base()
		st.idempotent = false
		if st.shouldRetryStatus(503) {
			t.Error("非幂等方法默认不应按状态码重试")
		}
	})

	t.Run("开启 retry_non_idempotent 后非幂等方法也重试", func(t *testing.T) {
		st := base()
		st.idempotent = false
		st.allowAnyRetry = true
		if !st.shouldRetryStatus(503) {
			t.Error("开启后非幂等方法应可按状态码重试")
		}
	})

	t.Run("请求体不可重放时不重试", func(t *testing.T) {
		st := base()
		st.replayable = false
		if st.shouldRetryStatus(503) {
			t.Error("请求体无法重放时不应重试")
		}
	})
}

func TestAttemptStateFailure(t *testing.T) {
	t.Run("无失败时返回 nil", func(t *testing.T) {
		st := attemptState{}
		if st.failure() != nil {
			t.Errorf("无失败时应返回 nil，实际 %v", st.failure())
		}
	})

	t.Run("优先返回传输错误", func(t *testing.T) {
		dial := dialError()
		st := attemptState{transportErr: dial, statusToRetry: 503}
		if !errors.Is(st.failure(), dial) {
			t.Errorf("应优先返回传输错误，实际 %v", st.failure())
		}
	})

	t.Run("仅有状态码时返回哨兵错误", func(t *testing.T) {
		st := attemptState{statusToRetry: 503}
		var statusErr retryableStatusError
		if !errors.As(st.failure(), &statusErr) || statusErr.status != 503 {
			t.Errorf("应返回 503 的哨兵错误，实际 %v", st.failure())
		}
	})
}
