// Package breaker 提供 agent 用的熔断器。
//
// 熔断只计「瞬时错误」：连接失败、超时、网关 5xx 这一类重试可能成功的事故。
// 业务错误（参数非法、库存不足、订单状态已变更）属于确定性错误，重试只会
// 重复同样的失败——把它们也计入会把熔断计数推到爆，反而把本来可用的调用
// 一起拉黑。
package breaker

import (
	"errors"
	"sync"
	"time"
)

// State 是熔断器的状态机。
type State int

const (
	// StateClosed 表示熔断器关闭，请求正常通过。
	StateClosed State = iota
	// StateOpen 表示熔断器打开，所有请求立即失败。
	StateOpen
	// StateHalfOpen 表示半开：允许放行一个探测请求验证流量是否恢复。
	StateHalfOpen
)

// Classifier 把错误分成"瞬时"与"业务"。
//
// 返回 true 表示瞬时错误（计入熔断），false 表示业务错误（不计入）。
// 默认实现见 IsClosedError。
type Classifier func(err error) bool

// IsClosedError 默认分类器：连接超时 / 5xx / 网关错误算瞬时，
// 其他全部算业务错误。
//
// 这是「保守」分类——未知错误的形态一律按业务错误处理，避免把
// 一个原本应该立刻暴露给用户的程序 bug 当作瞬时错误而吞掉。
func IsClosedError(err error) bool {
	if err == nil {
		return false
	}
	var te interface{ Timeout() bool }
	if errors.As(err, &te) && te.Timeout() {
		return true
	}
	var ne interface{ Temporary() bool }
	if errors.As(err, &ne) && ne.Temporary() {
		return true
	}
	// Sentinel：瞬时错误
	if errors.Is(err, ErrTransient) {
		return true
	}
	return false
}

// ErrTransient 是调用方主动声明的"瞬时错误"标记。
//
// 当一个外部错误不实现 Timeout/Temporary 接口，但语义上属于瞬时错误时，
// 调用方可以在 wrap 后追加 ErrTransient。
var ErrTransient = errors.New("breaker: 瞬时错误")

// ErrOpen 是熔断器打开时所有请求拿到的统一错误。
var ErrOpen = errors.New("breaker: 熔断中")

// Breaker is a circuit breaker.
type Breaker struct {
	mu             sync.Mutex
	state          State
	failures       int
	successesInHOP int // 半开窗口内累计的成功次数
	threshold      int
	halfOpenLimit  int
	openFor        time.Duration
	openedAt       time.Time
	classify       Classifier

	// Hooks for tests
	OnOpen     func()
	OnHalfOpen func()
	OnClose    func()
}

// New constructs a Breaker.
//
// threshold: 连续累计 threshold 个瞬时错误后熔断。
// openFor: 熔断打开的时长，到时进入半开。
// halfOpenLimit: 半开窗口允许放行的探测请求数；超过则继续半开。
func New(threshold int, openFor time.Duration, classify Classifier) *Breaker {
	if classify == nil {
		classify = IsClosedError
	}
	return &Breaker{
		state:         StateClosed,
		threshold:     threshold,
		openFor:       openFor,
		halfOpenLimit: 1,
		classify:      classify,
	}
}

// Allow reports whether a request may proceed right now.
//
// 当 state==Open 且 openFor 未到，返回 false 并返回 ErrOpen；
// 当 state==Open 且 openFor 已到，进入半开，允许一次探测。
func (b *Breaker) Allow() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		return nil
	case StateOpen:
		if time.Since(b.openedAt) >= b.openFor {
			b.transition(StateHalfOpen)
			b.successesInHOP = 0
			if b.OnHalfOpen != nil {
				b.OnHalfOpen()
			}
			return nil
		}
		return ErrOpen
	case StateHalfOpen:
		if b.successesInHOP+1 > b.halfOpenLimit {
			return ErrOpen
		}
		return nil
	}
	return nil
}

// Record reports the outcome of a request. Must be called after Allow.
//
// 业务错误（classify==false）一律不修改熔断器状态。
func (b *Breaker) Record(err error) {
	transient := b.classify(err)
	if !transient {
		// 业务错误：不修改熔断计数。
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateClosed:
		b.failures++
		if b.failures >= b.threshold {
			b.transition(StateOpen)
			b.openedAt = time.Now()
			if b.OnOpen != nil {
				b.OnOpen()
			}
		}
	case StateHalfOpen:
		// 半开窗口里又出现一次瞬时错误：重新熔断。
		b.transition(StateOpen)
		b.openedAt = time.Now()
		b.failures = b.threshold // 保持开态
		if b.OnOpen != nil {
			b.OnOpen()
		}
	}
}

// RecordSuccess 在一次请求成功后调用（用于半开窗口的探测成功）。
//
// 半开窗口累计 success >= halfOpenLimit 时，关闭熔断器。
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != StateHalfOpen {
		return
	}
	b.successesInHOP++
	if b.successesInHOP >= b.halfOpenLimit {
		b.transition(StateClosed)
		b.failures = 0
		if b.OnClose != nil {
			b.OnClose()
		}
	}
}

// State returns the current state. Test-only.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// Failures returns the current failure count. Test-only.
func (b *Breaker) Failures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures
}

func (b *Breaker) transition(s State) {
	b.state = s
}
