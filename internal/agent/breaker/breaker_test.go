package breaker

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// D4 验收：熔断只计瞬时错误，业务错误不应推高失败计数。
func TestBreaker_D4_DoesNotCountBusinessErrors(t *testing.T) {
	b := New(3, 100*time.Millisecond, IsClosedError)

	for i := 0; i < 10; i++ {
		_ = b.Allow()
		b.Record(errors.New("库存不足")) // 业务错误
	}
	if b.State() != StateClosed {
		t.Fatalf("业务错误不应推高熔断计数，state=%d", b.State())
	}
	if b.Failures() != 0 {
		t.Fatalf("业务错误不应增加 failure 计数，failures=%d", b.Failures())
	}
}

// 瞬时错误达到阈值后熔断打开。
func TestBreaker_OpensAfterTransientError(t *testing.T) {
	b := New(3, 100*time.Millisecond, IsClosedError)

	for i := 0; i < 3; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("closed 状态应放行，got %v", err)
		}
		b.Record(ErrTransient)
	}
	if b.State() != StateOpen {
		t.Fatalf("连续 3 个瞬时错误后应熔断，state=%d", b.State())
	}

	// 打开期间再次请求应被拒
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("open 状态应返回 ErrOpen，got %v", err)
	}
}

// open 状态到达 openFor 后进入 half-open，允许一次探测。
func TestBreaker_HalfOpenAfterTimeout(t *testing.T) {
	b := New(2, 30*time.Millisecond, IsClosedError)

	_ = b.Allow()
	b.Record(ErrTransient)
	_ = b.Allow()
	b.Record(ErrTransient)
	if b.State() != StateOpen {
		t.Fatalf("应已熔断，state=%d", b.State())
	}

	time.Sleep(50 * time.Millisecond)
	if err := b.Allow(); err != nil {
		t.Fatalf("half-open 应放行探测，got %v", err)
	}
	if b.State() != StateHalfOpen {
		t.Fatalf("应进入 half-open，state=%d", b.State())
	}

	// 探测成功 → closed
	b.RecordSuccess()
	if b.State() != StateClosed {
		t.Fatalf("探测成功后应回到 closed，state=%d", b.State())
	}
}

// half-open 探测又遇瞬时错误 → 重新熔断。
func TestBreaker_HalfOpenTransientReopens(t *testing.T) {
	b := New(2, 20*time.Millisecond, IsClosedError)
	_ = b.Allow()
	b.Record(ErrTransient)
	_ = b.Allow()
	b.Record(ErrTransient)
	time.Sleep(30 * time.Millisecond)
	_ = b.Allow() // → half-open
	if err := b.Allow(); err != nil {
		t.Fatalf("首次 half-open 应放行")
	}
	b.Record(ErrTransient)
	if b.State() != StateOpen {
		t.Fatalf("half-open 再瞬时失败应重新熔断，state=%d", b.State())
	}
}

// 业务错误与瞬时错误混合：业务错误被忽略，瞬时错误正常累加。
func TestBreaker_MixedErrorsOnlyCountsTransient(t *testing.T) {
	b := New(3, 100*time.Millisecond, IsClosedError)

	_ = b.Allow()
	b.Record(ErrTransient)
	_ = b.Allow()
	b.Record(errors.New("业务错误 A"))
	_ = b.Allow()
	b.Record(ErrTransient)
	_ = b.Allow()
	b.Record(errors.New("业务错误 B"))
	_ = b.Allow()
	b.Record(ErrTransient) // 累计 3 次瞬时错误

	if b.State() != StateOpen {
		t.Fatalf("3 次瞬时错误应熔断，业务错误未计入，state=%d", b.State())
	}
}

// Timeout 接口应被识别为瞬时。
func TestBreaker_TimeoutErrorIsTransient(t *testing.T) {
	b := New(2, 100*time.Millisecond, IsClosedError)
	_ = b.Allow()
	b.Record(errTimeout("连接超时"))
	_ = b.Allow()
	b.Record(errTimeout("再次超时"))
	if b.State() != StateOpen {
		t.Fatalf("timeout 错误应计为瞬时，state=%d", b.State())
	}
}

type errTimeout string

func (e errTimeout) Error() string   { return string(e) }
func (e errTimeout) Timeout() bool   { return true }
func (e errTimeout) Temporary() bool { return true }

// TestIsClosedError_BusinessError 验证普通业务错误返回 false。
func TestIsClosedError_BusinessError(t *testing.T) {
	err := errors.New("库存不足")
	if IsClosedError(err) {
		t.Error("业务错误应返回 false")
	}
	if IsClosedError(nil) {
		t.Error("nil 应返回 false")
	}
}

// TestIsClosedError_WrappedErrTransient 验证包装了 ErrTransient 的错误返回 true。
func TestIsClosedError_WrappedErrTransient(t *testing.T) {
	err := fmt.Errorf("连接失败: %w", ErrTransient)
	if !IsClosedError(err) {
		t.Error("包装了 ErrTransient 的错误应返回 true")
	}
}

// TestIsClosedError_Timeout 验证实现了 Timeout() 接口的错误返回 true。
func TestIsClosedError_Timeout(t *testing.T) {
	err := errTimeout("连接超时")
	if !IsClosedError(err) {
		t.Error("Timeout() 返回 true 的错误应被识别为瞬时错误")
	}
}

// TestIsClosedError_Temporary 验证仅实现 Temporary() 接口的错误返回 true。
func TestIsClosedError_Temporary(t *testing.T) {
	err := errTemporary("临时不可用")
	if !IsClosedError(err) {
		t.Error("Temporary() 返回 true 的错误应被识别为瞬时错误")
	}
}

// errTemporary 仅实现 Temporary 接口（不实现 Timeout）。
type errTemporary string

func (e errTemporary) Error() string   { return string(e) }
func (e errTemporary) Timeout() bool   { return false }
func (e errTemporary) Temporary() bool { return true }

// TestCircuitBreaker_Allow_OpenNotYetTime 验证 open 状态下时间未到时返回 ErrOpen。
func TestCircuitBreaker_Allow_OpenNotYetTime(t *testing.T) {
	b := New(2, 200*time.Millisecond, IsClosedError)

	_ = b.Allow()
	b.Record(ErrTransient)
	_ = b.Allow()
	b.Record(ErrTransient)

	// 立即再次 Allow，时间未到
	if err := b.Allow(); !errors.Is(err, ErrOpen) {
		t.Fatalf("open 状态时间未到应返回 ErrOpen，got %v", err)
	}
}

// TestCircuitBreaker_RecordSuccess_MultipleHalfOpen 验证 halfOpenLimit 硬编码为 1 的行为。
func TestCircuitBreaker_RecordSuccess_MultipleHalfOpen(t *testing.T) {
	b := New(2, 30*time.Millisecond, IsClosedError)

	_ = b.Allow()
	b.Record(ErrTransient)
	_ = b.Allow()
	b.Record(ErrTransient)
	time.Sleep(40 * time.Millisecond)

	// Allow → half-open
	if err := b.Allow(); err != nil {
		t.Fatalf("应进入 half-open，got %v", err)
	}
	// 此时 successesInHOP = 0, halfOpenLimit = 1

	// RecordSuccess 后应回到 closed
	b.RecordSuccess()
	if b.State() != StateClosed {
		t.Fatalf("首次半开探测成功后应回到 closed，state=%d", b.State())
	}

	// 再来一次：触发熔断
	_ = b.Allow()
	b.Record(ErrTransient)
	_ = b.Allow()
	b.Record(ErrTransient)
	time.Sleep(40 * time.Millisecond)

	_ = b.Allow() // → half-open
	b.RecordSuccess()
	if b.State() != StateClosed {
		t.Fatalf("第二次探测成功后也应回到 closed，state=%d", b.State())
	}
}

// TestCircuitBreaker_RecordSuccess_HalfOpenToClosed 验证半开探测成功后转回 closed。
func TestCircuitBreaker_RecordSuccess_HalfOpenToClosed(t *testing.T) {
	b := New(2, 30*time.Millisecond, IsClosedError)
	_ = b.Allow()
	b.Record(ErrTransient)
	_ = b.Allow()
	b.Record(ErrTransient)

	// 等待 openFor
	time.Sleep(40 * time.Millisecond)

	// Allow → half-open
	if err := b.Allow(); err != nil {
		t.Fatalf("应进入 half-open，got %v", err)
	}

	// 探测成功
	b.RecordSuccess()
	if b.State() != StateClosed {
		t.Fatalf("探测成功后应回到 closed，state=%d", b.State())
	}
}

// TestCircuitBreaker_RecordSuccess_NotHalfOpen 验证非 half-open 状态下 RecordSuccess 是空操作。
func TestCircuitBreaker_RecordSuccess_NotHalfOpen(t *testing.T) {
	b := New(2, 100*time.Millisecond, IsClosedError)

	// 仍在 closed 状态
	b.RecordSuccess()
	if b.State() != StateClosed {
		t.Fatalf("RecordSuccess 在 closed 状态下应为空操作，state=%d", b.State())
	}
}

// TestCircuitBreaker_Record_HalfOpenTransientReopen 验证 half-open 状态下 Record 瞬时错误重新熔断。
func TestCircuitBreaker_Record_HalfOpenTransientReopen(t *testing.T) {
	b := New(2, 30*time.Millisecond, IsClosedError)
	_ = b.Allow()
	b.Record(ErrTransient)
	_ = b.Allow()
	b.Record(ErrTransient)

	time.Sleep(40 * time.Millisecond)
	_ = b.Allow() // → half-open

	b.Record(ErrTransient) // 瞬时错误

	if b.State() != StateOpen {
		t.Fatalf("half-open 再瞬时失败应重新熔断，state=%d", b.State())
	}
}

// TestCircuitBreaker_New_ClassifyDefaults 验证 classify 为 nil 时使用默认分类器。
func TestCircuitBreaker_New_ClassifyDefaults(t *testing.T) {
	b := New(3, time.Second, nil)
	if b == nil {
		t.Fatal("New 不应返回 nil")
	}
	if b.State() != StateClosed {
		t.Fatalf("新 breaker 应处于 closed，state=%d", b.State())
	}
}

// TestCircuitBreaker_Allow_Closed 验证 closed 状态总是放行。
func TestCircuitBreaker_Allow_Closed(t *testing.T) {
	b := New(2, time.Second, IsClosedError)

	for i := 0; i < 5; i++ {
		if err := b.Allow(); err != nil {
			t.Fatalf("closed 状态应放行，got %v", err)
		}
	}
}

// TestCircuitBreaker_StateAndFailures 验证 State 和 Failures 读取。
func TestCircuitBreaker_StateAndFailures(t *testing.T) {
	b := New(3, time.Second, IsClosedError)

	if b.State() != StateClosed {
		t.Errorf("初始状态应为 closed，got %d", b.State())
	}
	if b.Failures() != 0 {
		t.Errorf("初始 failures 应为 0，got %d", b.Failures())
	}

	_ = b.Allow()
	b.Record(ErrTransient)
	if b.Failures() != 1 {
		t.Errorf("failures 应为 1，got %d", b.Failures())
	}
}
