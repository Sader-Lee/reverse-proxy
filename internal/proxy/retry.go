package proxy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Sader-Lee/reverse-proxy/internal/backend"
)

// maxReplayBodyBytes 是允许缓冲以便重放的请求体上限。
// 超过该上限的请求体不会被缓冲，该请求因此不会重试（但会照常转发）。
const maxReplayBodyBytes = 1 << 20

// Options 是转发行为的可调参数。零值表示：只尝试一次、不设单次超时、不按状态码重试。
type Options struct {
	// MaxAttempts 是含首次请求在内的总尝试次数，小于 1 时按 1 处理。
	MaxAttempts int
	// PerTryTimeout 是单次尝试的超时时间，超时按失败处理并回写 504；
	// 为 0 表示不额外限制（仍受 http.Server 与客户端超时约束）。
	PerTryTimeout time.Duration
	// RetryOnStatus 命中这些状态码时换实例重试（仅在还有剩余尝试次数时）。
	RetryOnStatus []int
	// RetryNonIdempotent 决定非幂等方法（POST/PATCH 等）是否也参与"完整重试"。
	// 为 false 时这类请求仅在连接都没建立起来（dial 失败）时才重试，
	// 避免请求可能已被后端处理的场景下重复提交。
	RetryNonIdempotent bool
}

// normalize 修正非法取值。
func (o Options) normalize() Options {
	if o.MaxAttempts < 1 {
		o.MaxAttempts = 1
	}
	if o.PerTryTimeout < 0 {
		o.PerTryTimeout = 0
	}
	return o
}

// retryStatusSet 把重试状态码列表转成集合，只在构造 Handler 时调用一次。
func (o Options) retryStatusSet() map[int]bool {
	if len(o.RetryOnStatus) == 0 {
		return nil
	}
	set := make(map[int]bool, len(o.RetryOnStatus))
	for _, code := range o.RetryOnStatus {
		set[code] = true
	}
	return set
}

// retryableStatusError 是"响应状态码需要重试"的哨兵错误：
// 由 ModifyResponse 返回，随后在 ErrorHandler 中被识别，不写回客户端。
type retryableStatusError struct {
	status int
}

// Error 实现 error。
func (e retryableStatusError) Error() string {
	return "需要重试的状态码 " + strconv.Itoa(e.status)
}

// attemptState 描述一次转发尝试，在尝试过程中被 ErrorHandler 与 ModifyResponse 读写。
// 每个尝试独占一个实例，因此字段无需加锁。
type attemptState struct {
	attempt     int
	maxAttempts int
	backend     *backend.Backend
	retryStatus map[int]bool

	// 请求是否允许重试：非幂等方法、无法重放的请求体会收紧策略
	idempotent    bool
	allowAnyRetry bool // 对应 Options.RetryNonIdempotent
	replayable    bool // 请求体可被重放

	// 以下是本次尝试的结果
	retried        bool  // 决定换实例重试
	clientCanceled bool  // 客户端主动断开
	statusToRetry  int   // 触发重试的状态码，0 表示非状态码触发
	transportErr   error // 传输层错误
}

// failure 返回本次尝试的失败原因，没有失败时返回 nil。
func (s *attemptState) failure() error {
	if s.transportErr != nil {
		return s.transportErr
	}
	if s.statusToRetry != 0 {
		return retryableStatusError{status: s.statusToRetry}
	}
	return nil
}

// canRetryTransport 判断传输层错误是否可以换实例重试。
func (s *attemptState) canRetryTransport(err error) bool {
	// 请求体无法重放时只能尝试一次
	if !s.replayable {
		return false
	}
	if s.idempotent || s.allowAnyRetry {
		return true
	}
	// 非幂等方法：仅当连接都没建立起来（请求肯定没被后端处理）时才安全重试
	return isDialError(err)
}

// shouldRetryStatus 判断该状态码是否应触发重试。
func (s *attemptState) shouldRetryStatus(status int) bool {
	if s.attempt >= s.maxAttempts || !s.replayable {
		return false
	}
	if !s.idempotent && !s.allowAnyRetry {
		return false
	}
	return s.retryStatus[status]
}

// isIdempotent 判断请求方法是否幂等（可安全重放）。
func isIdempotent(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete,
		http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// isDialError 判断错误是否发生在建立连接阶段（请求尚未发出）。
func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return opErr.Op == "dial"
	}
	return false
}

// replayBody 把"已读出的前缀"与"剩余流"拼回一个 ReadCloser。
type replayBody struct {
	io.Reader
	closer io.Closer
}

// Close 实现 io.Closer。
func (b *replayBody) Close() error { return b.closer.Close() }

// readReplayBody 尝试把请求体读入内存，以便重试时重放。
//
// 返回 (body, replayable)：
//   - replayable 为 true 时，body 即完整请求体（可能为空表示没有请求体）；
//   - replayable 为 false 时，请求体过大或读取失败，此时会把已读部分与剩余流拼回去，
//     调用方必须继续使用 r.Body 转发，且不得重试该请求。
func readReplayBody(r *http.Request) ([]byte, bool) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, true
	}

	buf, err := io.ReadAll(io.LimitReader(r.Body, maxReplayBodyBytes+1))
	if err != nil || len(buf) > maxReplayBodyBytes {
		// 拼回原流，保证本次转发仍能读到完整请求体
		r.Body = &replayBody{
			Reader: io.MultiReader(bytes.NewReader(buf), r.Body),
			closer: r.Body,
		}
		return nil, false
	}
	return buf, true
}
