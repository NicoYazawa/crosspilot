package breaker

import (
	"errors"
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

	b.Allow()
	b.Record(ErrTransient)
	b.Allow()
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
	b.Allow()
	b.Record(ErrTransient)
	b.Allow()
	b.Record(ErrTransient)
	time.Sleep(30 * time.Millisecond)
	b.Allow() // → half-open
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

	b.Allow()
	b.Record(ErrTransient)
	b.Allow()
	b.Record(errors.New("业务错误 A"))
	b.Allow()
	b.Record(ErrTransient)
	b.Allow()
	b.Record(errors.New("业务错误 B"))
	b.Allow()
	b.Record(ErrTransient) // 累计 3 次瞬时错误

	if b.State() != StateOpen {
		t.Fatalf("3 次瞬时错误应熔断，业务错误未计入，state=%d", b.State())
	}
}

// Timeout 接口应被识别为瞬时。
func TestBreaker_TimeoutErrorIsTransient(t *testing.T) {
	b := New(2, 100*time.Millisecond, IsClosedError)
	b.Allow()
	b.Record(errTimeout("连接超时"))
	b.Allow()
	b.Record(errTimeout("再次超时"))
	if b.State() != StateOpen {
		t.Fatalf("timeout 错误应计为瞬时，state=%d", b.State())
	}
}

type errTimeout string

func (e errTimeout) Error() string   { return string(e) }
func (e errTimeout) Timeout() bool   { return true }
func (e errTimeout) Temporary() bool { return true }
