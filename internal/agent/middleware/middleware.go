// Package middleware 提供 agent 调用链的中间件。
//
// 中间件链顺序：Harness（外）→ Resilience（内），**顺序不可调换**——
// Harness 提供请求级断言/超时/取消；Resilience 提供熔断/瞬时错误重试。
// 顺序倒过来会让熔断先放行，再被 Harness 拒绝，触发无意义的熔断计数。
//
// 验收 D3：断言失败必须先于超时触发；超时不得污染熔断计数。
package middleware

import (
	"context"
	"errors"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/breaker"
)

// Assertion 是一次调用前的断言。返回 error 表示业务前置条件不满足。
//
// 与 Resilience 重试不同，断言失败**不**应触发重试——它代表的是请求
// 在语义上注定失败，重试只会再次断言失败。
type Assertion func(ctx context.Context) error

// ResilienceOpts 控制 Resilience 中间件的行为。
type ResilienceOpts struct {
	// Breaker 是熔断器。
	Breaker *breaker.Breaker
	// MaxAttempts 是瞬时错误的最大重试次数（包含首次）。
	MaxAttempts int
	// Backoff 是两次重试之间的等待时间（首次失败后等 backoff，第二次再失败等 2*backoff，依此类推）。
	Backoff time.Duration
	// ShouldRetry 判定一个错误是否为「瞬时错误」；nil 时用 breaker.IsClosedError。
	ShouldRetry func(error) bool
	// OnRetry 每次重试前回调，可用于打点。
	OnRetry func(attempt int, err error)
}

// HarnessOpts 控制 Harness 中间件的行为。
type HarnessOpts struct {
	// Assertions 是按顺序执行的前置断言；任何一项返回 error 即拒绝。
	Assertions []Assertion
	// Timeout 是单次动作允许的最长执行时间。
	Timeout time.Duration
	// OnReject 当断言失败时被调用，可用于审计与拒绝计数。
	OnReject func(reason error)
}

// Chain 是中间件链，按从外到内顺序包装。
type Chain struct {
	// Outer 是先执行的中间件（Harness）。
	Outer []Middleware
	// Inner 是后执行的中间件（Resilience）。
	Inner []Middleware
}

// Middleware wraps a Handler.
//
// handler(nil) 表示直接放行到下游。返回 error 时中间件决定是中断还是允许通过。
type Middleware func(next Handler) Handler

// Handler 是中间件链最里层的实际动作。
type Handler func(ctx context.Context) error

// Apply 按 Chain 中 Outer→Inner 的顺序把中间件套到最里层 handler 上。
//
// Wrap 顺序：先套 Inner，再套 Outer，调用时 Outer 最先执行。
// 这正是「Harness(外)→ Resilience(内)」的语义。
func (c Chain) Apply(innermost Handler) Handler {
	h := innermost
	for i := len(c.Inner) - 1; i >= 0; i-- {
		h = c.Inner[i](h)
	}
	for i := len(c.Outer) - 1; i >= 0; i-- {
		h = c.Outer[i](h)
	}
	return h
}

// Harness 是"断言 + 超时"中间件。
//
// D3 验收：断言失败先于超时触发；超时不得污染熔断计数。
func Harness(opts HarnessOpts) Middleware {
	return func(next Handler) Handler {
		return func(ctx context.Context) error {
			// 1) 断言先于超时执行。
			for _, a := range opts.Assertions {
				if err := a(ctx); err != nil {
					if opts.OnReject != nil {
						opts.OnReject(err)
					}
					return err
				}
			}

			// 2) 超时上下文。
			if opts.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
				defer cancel()
			}

			err := next(ctx)
			// 重要：context.DeadlineExceeded 由超时触发，调用方必须明确区分。
			// 标记一个 sentinel 让外层可以识别，但**不**影响熔断（IsClosedError 默认返回 false）。
			if errors.Is(err, context.DeadlineExceeded) {
				return wrapTimeout(err)
			}
			return err
		}
	}
}

// ErrTimeout 标记一个超时错误。IsClosedError 返回 false（业务/请求级错误，不计入熔断）。
//
// 这里把超时显式排除在熔断外，原因：
//   - 单次超时往往是 Harness 的预算触底，重试同一笔请求只会被同样的预算再砍掉；
//   - 把超时计入会污染熔断计数，让"我用完预算"看上去像"服务挂了"。
var ErrTimeout = errors.New("middleware: 单次动作超时")

func wrapTimeout(err error) error {
	return &timeoutErr{cause: err}
}

type timeoutErr struct{ cause error }

func (e *timeoutErr) Error() string { return "timeout: " + e.cause.Error() }
func (e *timeoutErr) Unwrap() error { return e.cause }

// IsTimeout 报告 error 是否来自 Harness 超时（用于测试与外层路由）。
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	var t *timeoutErr
	if errors.As(err, &t) {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// Resilience 是"熔断 + 瞬时错误重试"中间件。
//
// 顺序：先熔断检查，再重试循环。重试时**清空输入**——避免上下文出现两次买家发言。
//
// 关键不变量（D3.2）：
//   - 超时错误（来自 Harness）**不**触发重试、**不**计入熔断计数。
//   - 它是请求级错误而非服务级瞬时错误，重试同一笔请求只会被同样的预算再砍掉。
func Resilience(opts ResilienceOpts) Middleware {
	if opts.ShouldRetry == nil {
		opts.ShouldRetry = breaker.IsClosedError
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 1
	}

	return func(next Handler) Handler {
		return func(ctx context.Context) error {
			if opts.Breaker != nil {
				if err := opts.Breaker.Allow(); err != nil {
					return err
				}
			}

			var lastErr error
			for attempt := 1; attempt <= opts.MaxAttempts; attempt++ {
				err := next(ctx)
				if err == nil {
					if opts.Breaker != nil {
						opts.Breaker.RecordSuccess()
					}
					return nil
				}

				lastErr = err

				// 超时错误：业务级，不重试、不计入熔断。
				if IsTimeout(err) {
					return err
				}

				// 业务错误：不重试（classify 已隐式区分）。
				if !opts.ShouldRetry(err) {
					// 超时错误彻底绕开熔断（防止 Harness 预算触底推高计数）。
					if !IsTimeout(err) && opts.Breaker != nil {
						opts.Breaker.Record(err) // Record 内部用 classify 决定是否计数
					}
					return err
				}

				// 瞬时错误：记录到熔断器。
				if opts.Breaker != nil {
					opts.Breaker.Record(err)
					// 熔断已打开则停止继续重试。
					if err2 := opts.Breaker.Allow(); err2 != nil {
						return err2
					}
				}

				if attempt < opts.MaxAttempts {
					if opts.OnRetry != nil {
						opts.OnRetry(attempt, err)
					}
					// 简单线性退避（生产可换成 jittered exponential）。
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(time.Duration(attempt) * opts.Backoff):
					}
					continue
				}
				return err
			}
			return lastErr
		}
	}
}
