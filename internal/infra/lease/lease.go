// Package lease 提供带租期的会话执行权。
//
// 它解决的是「同一个会话被两个执行者同时处理」：执行者先抢占一个带过期的键，
// 抢到才能开工，并在租期内周期性续约。抢不到就等，等不到就明确失败，
// 而不是无限排队。
//
// 租约不是权威判据。Redis 可能因为网络分区、主从切换或进程长时间暂停而
// 认为租约已经易主，而持有者并不知情。因此真正的最终防线在持久层：
// 会话写入用 session_id + owner + revision + fence 条件更新，
// 旧执行者的写入一定会被拒绝。租约的作用是把这种情况变成「尽早发现」，
// 而不是「唯一保证」。
package lease

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// 会话租约的默认参数。
const (
	// DefaultTTL 是租期。取得执行权后超过这个时长没有续约，键就会过期。
	DefaultTTL = 30 * time.Second

	// DefaultHeartbeat 是续约间隔。
	//
	// 必须明显小于租期：一次续约可能因为网络抖动或进程调度而失败或被推迟，
	// 间隔越接近租期，这种抖动就越容易造成「明明还活着却被判失去执行权」。
	DefaultHeartbeat = 5 * time.Second

	// DefaultWaitTimeout 是抢占失败后的等待上限。
	DefaultWaitTimeout = 30 * time.Second

	// retryInterval 是抢占失败后的轮询间隔。
	retryInterval = 100 * time.Millisecond
)

// 租约相关的错误。
var (
	// ErrLost 表示执行权已经失去。
	ErrLost = errors.New("lease: 会话执行权已失去")

	// ErrTimeout 表示在等待上限内没有抢到执行权。
	//
	// 这是一个明确的失败信号：调用方应当告诉用户「稍后重试」，
	// 而不是继续排队——无限等待会把一次拥塞变成一次积压。
	ErrTimeout = errors.New("lease: 当前会话仍在处理中，请稍后重试")

	// ErrNotHeld 表示对已释放的租约调用了续约或释放。
	ErrNotHeld = errors.New("lease: 租约未持有")
)

// Backend 是租约的存储后端。
//
// 三个方法都必须是原子的：Acquire 用「不存在才设置」，Renew 用
// 「值仍是自己的才延长」，Release 用「值仍是自己的才删除」。
// 拆成「先读、再写」就会留下窗口，让两个执行者都认为自己持有执行权。
type Backend interface {
	// Acquire 在键不存在时以 owner 与 ttl 建立租约，返回是否抢到。
	Acquire(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)

	// Renew 在键的值仍为 owner 时把过期时间重置为 ttl，返回是否续约成功。
	Renew(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)

	// Release 在键的值仍为 owner 时删除它，返回是否删除成功。
	Release(ctx context.Context, key, owner string) (bool, error)

	// Get 返回键当前的值；键不存在时第二个返回值为 false。
	Get(ctx context.Context, key string) (string, bool, error)
}

// Clock 提供当前时间，便于测试固定时间推进。
type Clock interface {
	Now() time.Time
}

// Options 是 Manager 的构造参数。
type Options struct {
	Backend Backend
	Prefix  string
	TTL     time.Duration
	// Heartbeat 必须小于 TTL；为零时取 DefaultHeartbeat。
	Heartbeat time.Duration
	// WaitTimeout 为零时取 DefaultWaitTimeout。
	WaitTimeout time.Duration
	Clock       Clock
	// Logf 记录租约生命周期事件；为 nil 时静默。
	Logf func(format string, args ...any)
}

// Manager 按会话标识发放租约。
//
// 同一个 Manager 在同一会话上重复抢占会复用已持有的租约，
// 而不是再去抢一次：重入不应该被自己挡住。
type Manager struct {
	backend     Backend
	prefix      string
	ttl         time.Duration
	heartbeat   time.Duration
	waitTimeout time.Duration
	clock       Clock
	logf        func(format string, args ...any)

	mu   sync.Mutex
	held map[string]*Lease
}

// New 构造租约管理器。
func New(opts Options) (*Manager, error) {
	if opts.Backend == nil {
		return nil, errors.New("lease: 缺少存储后端")
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	heartbeat := opts.Heartbeat
	if heartbeat <= 0 {
		heartbeat = DefaultHeartbeat
	}
	// 心跳必须严格小于租期，否则一次续约还没完成租约就已经到期，
	// 续约会持续失败而持有者却以为什么都没发生。
	if heartbeat >= ttl {
		return nil, fmt.Errorf("lease: 续约间隔 %s 必须小于租期 %s", heartbeat, ttl)
	}
	wait := opts.WaitTimeout
	if wait <= 0 {
		wait = DefaultWaitTimeout
	}

	manager := &Manager{
		backend:     opts.Backend,
		prefix:      opts.Prefix,
		ttl:         ttl,
		heartbeat:   heartbeat,
		waitTimeout: wait,
		clock:       opts.Clock,
		logf:        opts.Logf,
		held:        make(map[string]*Lease),
	}
	if manager.prefix == "" {
		manager.prefix = "crosspilot:lease:session:"
	}
	if manager.clock == nil {
		manager.clock = wallClock{}
	}
	// 日志是可选的：装配点不传日志不应该让进程在第一次抢占成功时 panic。
	// 默认给一个空实现，而不是在每个调用点判空——判空只要漏一处就是崩溃。
	if manager.logf == nil {
		manager.logf = func(string, ...any) {}
	}
	return manager, nil
}

// TTL 返回租期。
func (m *Manager) TTL() time.Duration { return m.ttl }

// Heartbeat 返回续约间隔。
func (m *Manager) Heartbeat() time.Duration { return m.heartbeat }

// Acquire 抢占会话执行权。
//
// 同一个 Manager 已经持有该会话的租约时直接复用：重入不应该被自己挡住，
// 否则一个请求内部的两段逻辑会互相等待到超时。
func (m *Manager) Acquire(ctx context.Context, sessionID string) (*Lease, error) {
	m.mu.Lock()
	if existing, ok := m.held[sessionID]; ok && existing.Valid() {
		m.mu.Unlock()
		return existing, nil
	}
	m.mu.Unlock()

	owner, err := newOwner()
	if err != nil {
		return nil, err
	}
	key := m.prefix + sessionID

	deadline := m.clock.Now().Add(m.waitTimeout)
	for {
		acquired, acquireErr := m.backend.Acquire(ctx, key, owner, m.ttl)
		if acquireErr != nil {
			return nil, fmt.Errorf("lease: 抢占会话执行权失败: %w", acquireErr)
		}
		if acquired {
			l := newLease(m, sessionID, key, owner)
			m.mu.Lock()
			m.held[sessionID] = l
			m.mu.Unlock()
			m.logf("租约已取得 session=%s owner=%s", sessionID, owner)
			return l, nil
		}

		if !m.clock.Now().Before(deadline) {
			return nil, ErrTimeout
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryInterval):
		}
	}
}

// forget 从持有表中移除一个租约。
func (m *Manager) forget(sessionID string, l *Lease) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if current, ok := m.held[sessionID]; ok && current == l {
		delete(m.held, sessionID)
	}
}

// Held 报告当前是否持有该会话的执行权。
func (m *Manager) Held(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.held[sessionID]
	return ok && l.Valid()
}

// Lease 是一次会话执行权的持有凭证。
type Lease struct {
	manager   *Manager
	SessionID string
	Key       string
	Owner     string

	mu          sync.Mutex
	valid       bool
	released    bool
	lastRenew   time.Time
	onLost      []func()
	stopWatch   chan struct{}
	watchClosed bool
}

func newLease(manager *Manager, sessionID, key, owner string) *Lease {
	return &Lease{
		manager:   manager,
		SessionID: sessionID,
		Key:       key,
		Owner:     owner,
		valid:     true,
		lastRenew: manager.clock.Now(),
		stopWatch: make(chan struct{}),
	}
}

// Valid 报告租约是否仍然有效。
//
// 它读的是本地状态，因此这是一个「快速拦截」而不是权威判断：
// 网络分区时本地仍会认为自己有效，最终裁决在持久层的条件更新。
func (l *Lease) Valid() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.valid
}

// OwnerToken 返回本次持有者的唯一标识，供持久层 fence 使用。
func (l *Lease) OwnerToken() string { return l.Owner }

// OnLost 注册租约丢失时的回调。
//
// 回调按注册顺序同步执行，且只执行一次：它们要做的是「取消生产者」
// 这类不可重复的动作，被调用两次会让取消变成两次互相干扰的收尾。
func (l *Lease) OnLost(callback func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.onLost = append(l.onLost, callback)
}

// Invalidate 立即作废本地租约并触发丢失回调。
//
// 顺序是刻意的：先把本地标记为无效，再通知回调。反过来会让回调观察到
// 「租约仍然有效」的瞬间状态，进而基于错误判断继续写入。
//
// valid 同时充当「回调是否已触发」的闩锁：入口处对它判一次，就保证回调
// 只可能被触发一次，不需要再维护一个独立的标记位。
func (l *Lease) Invalidate() {
	l.mu.Lock()
	if !l.valid {
		l.mu.Unlock()
		return
	}
	l.valid = false
	callbacks := make([]func(), len(l.onLost))
	copy(callbacks, l.onLost)
	l.mu.Unlock()

	l.manager.forget(l.SessionID, l)
	l.manager.logf("租约已失效 session=%s owner=%s", l.SessionID, l.Owner)
	for _, callback := range callbacks {
		callback()
	}
}

// Renew 续约一次。返回 false 表示执行权已经不在自己手上。
func (l *Lease) Renew(ctx context.Context) (bool, error) {
	l.mu.Lock()
	if !l.valid {
		l.mu.Unlock()
		return false, ErrNotHeld
	}
	l.mu.Unlock()

	ok, err := l.manager.backend.Renew(ctx, l.Key, l.Owner, l.manager.ttl)
	if err != nil {
		return false, fmt.Errorf("lease: 续约失败: %w", err)
	}
	if !ok {
		// 键还在，但值已经不是自己的：说明执行权被别人接管
		l.Invalidate()
		return false, ErrLost
	}

	l.mu.Lock()
	l.lastRenew = l.manager.clock.Now()
	l.mu.Unlock()
	return true, nil
}

// Watch 启动后台续约，直到 ctx 结束或租约失效。
//
// 每次心跳做两件事：续约，以及检查「距离上次成功续约已经过了多久」。
// 后者对应「本进程长时间暂停」这一情形——暂停期间续约没有发生，
// 租约可能已经易主；恢复后与其继续写，不如先把自己作废。
func (l *Lease) Watch(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(l.manager.heartbeat)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-l.stopWatch:
				return
			case <-ticker.C:
				if !l.Valid() {
					return
				}
				// 超过两个心跳没有成功续约，先按失去执行权处理；
				// 若其实只是被调度推迟，本地作废也不会写坏数据——
				// 持久层本来就要求票据匹配，重新抢占即可。
				if l.staleFor() > 2*l.manager.heartbeat {
					l.Invalidate()
					return
				}
				if _, err := l.Renew(ctx); err != nil {
					l.Invalidate()
					return
				}
			}
		}
	}()
}

// staleFor 返回距离上次成功续约经过的时间。
func (l *Lease) staleFor() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.manager.clock.Now().Sub(l.lastRenew)
}

// Release 归还执行权。
//
// 只删除值仍是自己的键：一个已经被别人接管的租约，释放时不能把
// 新持有者的键删掉，否则会把互斥彻底打穿。
func (l *Lease) Release(ctx context.Context) error {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	alreadyInvalid := !l.valid
	// 归还即失效。只置 released 而不清 valid 会留下一个「已归还但仍然有效」的
	// 中间态：此后续约会打到后端并成功，于是一次正常结束的执行被算成
	// 「还持有执行权」，而真正抢到键的人反被延长了别人的租约。
	l.valid = false
	if !l.watchClosed {
		close(l.stopWatch)
		l.watchClosed = true
	}
	l.mu.Unlock()

	l.manager.forget(l.SessionID, l)
	if alreadyInvalid {
		return nil
	}

	released, err := l.manager.backend.Release(ctx, l.Key, l.Owner)
	if err != nil {
		// 释放失败只记录：键会自然过期，等 TTL 回收即可，
		// 把释放失败当成业务失败会让已经完成的交易被误报为失败。
		l.manager.logf("租约释放失败，等待过期回收 session=%s: %v", l.SessionID, err)
		return fmt.Errorf("lease: 释放租约失败: %w", err)
	}
	if !released {
		l.manager.logf("租约已被他人接管，未删除其键 session=%s", l.SessionID)
	}
	return nil
}

// newOwner 生成持有者标识：16 字节随机数的十六进制。
func newOwner() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("lease: 生成持有者标识失败: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// wallClock 是默认时钟。
type wallClock struct{}

// Now 返回当前时间。
func (wallClock) Now() time.Time { return time.Now() }
