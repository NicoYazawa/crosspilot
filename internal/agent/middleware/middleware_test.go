package middleware

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/breaker"
)

// D3.1 验收：断言失败必须先于超时触发。
func TestD3_AssertionRejectionBeforeTimeout(t *testing.T) {
	var handlerRan atomic.Bool

	// 模拟一个"耗时 200ms 但先有断言"的动作；断言会立刻拒绝。
	h := Chain{
		Outer: []Middleware{Harness(HarnessOpts{
			Assertions: []Assertion{
				func(_ context.Context) error { return errors.New("断言拒绝") },
			},
			Timeout: 200 * time.Millisecond,
		})},
	}.Apply(func(_ context.Context) error {
		handlerRan.Store(true)
		return nil
	})

	start := time.Now()
	err := h(context.Background())
	elapsed := time.Since(start)

	if err == nil || err.Error() != "断言拒绝" {
		t.Fatalf("应返回断言错误，got %v", err)
	}
	if handlerRan.Load() {
		t.Fatal("断言失败时不应执行 handler")
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("断言失败应立即返回，elapsed=%v", elapsed)
	}
}

// D3.2 验收：超时不得污染熔断计数。
func TestD3_TimeoutDoesNotPolluteBreaker(t *testing.T) {
	br := breaker.New(2, time.Second, breaker.IsClosedError)

	// 模拟 handler：每次都触发 timeout
	slowHandler := func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
			return nil
		}
	}

	h := Chain{
		Outer: []Middleware{Harness(HarnessOpts{Timeout: 10 * time.Millisecond})},
		Inner: []Middleware{Resilience(ResilienceOpts{
			Breaker:     br,
			MaxAttempts: 1,
		})},
	}.Apply(slowHandler)

	for i := 0; i < 5; i++ {
		err := h(context.Background())
		if !IsTimeout(err) {
			t.Fatalf("第 %d 次应返回超时错误，got %v", i+1, err)
		}
	}

	if br.State() != breaker.StateClosed {
		t.Fatalf("超时不应打开熔断，state=%d", br.State())
	}
	if br.Failures() != 0 {
		t.Fatalf("超时不应累计 failure 计数，failures=%d", br.Failures())
	}
}

// 顺序验收：Harness 在 Resilience 之外。
// 即：超时由 Harness 设置；瞬时错误由 Resilience 重试。
// 验证 Resilience 看到的是已经带 timeout 的 ctx，而不是更长。
func TestD3_OrderIsHarnessOuterResilienceInner(t *testing.T) {
	var handlerSeenTimeout atomic.Bool

	inner := func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); ok {
			handlerSeenTimeout.Store(true)
		}
		return nil
	}

	chain := Chain{
		Outer: []Middleware{Harness(HarnessOpts{Timeout: 30 * time.Millisecond})},
		Inner: []Middleware{Resilience(ResilienceOpts{
			Breaker:     breaker.New(10, time.Second, breaker.IsClosedError),
			MaxAttempts: 1,
		})},
	}
	h := chain.Apply(inner)

	if err := h(context.Background()); err != nil {
		t.Fatalf("handler 应正常完成，got %v", err)
	}
	if !handlerSeenTimeout.Load() {
		t.Fatal("handler 应该看到 Harness 设置的 deadline")
	}
}

// Resilience 重试：瞬时错误应被重试，业务错误不应重试。
func TestResilience_RetriesTransientOnly(t *testing.T) {
	var calls atomic.Int32

	br := breaker.New(10, time.Second, breaker.IsClosedError)

	h := Chain{
		Inner: []Middleware{Resilience(ResilienceOpts{
			Breaker:     br,
			MaxAttempts: 3,
			Backoff:     1 * time.Millisecond,
		})},
	}.Apply(func(_ context.Context) error {
		n := calls.Add(1)
		if n < 2 {
			return breaker.ErrTransient
		}
		return nil
	})

	if err := h(context.Background()); err != nil {
		t.Fatalf("最终应成功，got %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("应调用 2 次（首次失败 + 重试成功），got %d", calls.Load())
	}
}

// 业务错误不应重试：第一次失败立即返回。
func TestResilience_NoRetryOnBusinessError(t *testing.T) {
	var calls atomic.Int32
	business := errors.New("参数非法")

	h := Chain{
		Inner: []Middleware{Resilience(ResilienceOpts{
			Breaker:     breaker.New(10, time.Second, breaker.IsClosedError),
			MaxAttempts: 3,
		})},
	}.Apply(func(_ context.Context) error {
		calls.Add(1)
		return business
	})

	err := h(context.Background())
	if !errors.Is(err, business) {
		t.Fatalf("应返回原错误，got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("业务错误不应重试，calls=%d", calls.Load())
	}
}

// Resilience 熔断触发时立即返回。
func TestResilience_BreakerTrips(t *testing.T) {
	br := breaker.New(2, 5*time.Second, breaker.IsClosedError)

	h := Chain{
		Inner: []Middleware{Resilience(ResilienceOpts{
			Breaker:     br,
			MaxAttempts: 3,
			Backoff:     1 * time.Millisecond,
		})},
	}.Apply(func(_ context.Context) error {
		return breaker.ErrTransient
	})

	_ = h(context.Background()) // 第 1 次失败
	_ = h(context.Background()) // 第 2 次失败（累计 2）

	err := h(context.Background())
	if !errors.Is(err, breaker.ErrOpen) {
		t.Fatalf("熔断后应返回 ErrOpen，got %v", err)
	}
}

// TestTimeoutError_Error_Unwrap 验证 timeoutErr 的 Error 和 Unwrap 方法。
func TestTimeoutError_Error_Unwrap(t *testing.T) {
	inner := context.DeadlineExceeded
	wrapped := wrapTimeout(inner)

	if wrapped == nil {
		t.Fatal("wrapTimeout 不应返回 nil")
	}
	if !errors.Is(wrapped, context.DeadlineExceeded) {
		t.Error("Unwrap 应返回原始 context.DeadlineExceeded")
	}

	// unwrapped 再次 unwrap
	if !errors.Is(errors.Unwrap(wrapped), inner) {
		t.Error("Unwrap 层级不对")
	}
}

// TestIsTimeout_DirectDeadlineExceeded 验证 IsTimeout 能识别直接传入的 DeadlineExceeded。
func TestIsTimeout_DirectDeadlineExceeded(t *testing.T) {
	if !IsTimeout(context.DeadlineExceeded) {
		t.Error("IsTimeout 应识别 context.DeadlineExceeded")
	}
	if IsTimeout(nil) {
		t.Error("IsTimeout 对 nil 应返回 false")
	}
	if IsTimeout(errors.New("其他错误")) {
		t.Error("IsTimeout 对普通错误应返回 false")
	}
}

// TestIsTimeout_WrappedTimeout 验证 IsTimeout 能识别包装后的超时错误。
func TestIsTimeout_WrappedTimeout(t *testing.T) {
	wrapped := wrapTimeout(context.DeadlineExceeded)
	if !IsTimeout(wrapped) {
		t.Error("IsTimeout 应识别包装后的超时错误")
	}
}

// TestResilience_TimeoutReturnsWrappedError 验证超时错误被正确包装后返回。
func TestResilience_TimeoutReturnsWrappedError(t *testing.T) {
	h := Chain{
		Outer: []Middleware{Harness(HarnessOpts{Timeout: 10 * time.Millisecond})},
		Inner: []Middleware{Resilience(ResilienceOpts{
			MaxAttempts: 1,
		})},
	}.Apply(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
			return nil
		}
	})

	err := h(context.Background())
	if !IsTimeout(err) {
		t.Fatalf("超时错误应被包装，got %v", err)
	}
}

// TestResilience_MaxAttemptsDefaultsToOne 验证 MaxAttempts <= 0 时默认为 1。
func TestResilience_MaxAttemptsDefaultsToOne(t *testing.T) {
	var calls int
	h := Chain{
		Inner: []Middleware{Resilience(ResilienceOpts{
			MaxAttempts: 0,
			Backoff:     1 * time.Millisecond,
		})},
	}.Apply(func(_ context.Context) error {
		calls++
		return breaker.ErrTransient
	})

	_ = h(context.Background())
	// 0 被当作 1，所以只调用一次
	if calls != 1 {
		t.Errorf("MaxAttempts=0 应默认为 1，只调用 1 次，got %d", calls)
	}
}

// TestHarness_ZeroTimeout 验证 Timeout=0 时不设置超时上下文。
func TestHarness_ZeroTimeout(t *testing.T) {
	var hasDeadline atomic.Bool
	h := Chain{
		Outer: []Middleware{Harness(HarnessOpts{Timeout: 0})},
	}.Apply(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); ok {
			hasDeadline.Store(true)
		}
		return nil
	})

	if err := h(context.Background()); err != nil {
		t.Fatalf("不应报错，got %v", err)
	}
	if hasDeadline.Load() {
		t.Error("Timeout=0 时 ctx 不应有 deadline")
	}
}

// TestResilience_ContextCancelled 验证 ctx 被取消时立即返回。
func TestResilience_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 先取消

	h := Chain{
		Inner: []Middleware{Resilience(ResilienceOpts{
			MaxAttempts: 3,
			Backoff:     10 * time.Millisecond,
		})},
	}.Apply(func(_ context.Context) error {
		return breaker.ErrTransient
	})

	err := h(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 已取消应返回 Canceled，got %v", err)
	}
}
