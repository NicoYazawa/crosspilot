package lease

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件用「假后端 + 假时钟」覆盖租约的全部本地行为。
//
// 为什么不用真 Redis：这里要守的是「本地是否认为执行权还在自己手上」，
// 与 Redis 的读写无关；换成假后端后，抢占失败、续约被拒、键被接管这些
// 分支都能被精确摆出来，而不用靠真实网络去凑。
//
// 为什么用假时钟：staleFor 与 WaitTimeout 都读 Clock，用真实时间验证
// 就只能靠 Sleep 凑时长，机器一忙就变红。假时钟让时间成为测试手里的变量。

// errBackendDown 是替身后端返回的底层错误。
//
// 租约必须把它包起来而不是吞掉或替换：调用方要靠 errors.Is 区分
// 「后端不可用」（该降级到单进程模式）与「没抢到」（该告诉用户稍后重试）。
var errBackendDown = errors.New("后端不可用")

// noopLogf 是默认的静默日志。
//
// 必须显式传入：lease 目前没有把 nil Logf 兜底成空实现，
// 生命周期事件一触发就会调用 nil 函数值并 panic
// （见 TestNilLogfPanicsOnAcquire 与 Options.Logf 的注释不一致之处）。
var noopLogf = func(string, ...any) {}

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// fakeClock 是可手动推进的时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	// 固定起点：断言里出现时间时不会随运行时刻变化
	return &fakeClock{now: time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 推进时钟。只有测试协程会推进它，
// 因此「陈旧程度」这类断言不受协程调度快慢影响。
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeBackend 是内存里的租约存储。
//
// 三个方法都通过 hook 决定返回值，便于把「第一次拒绝、第二次成功」
// 「续约时发现键已易主」这类时序精确摆出来；调用次数与删除记录
// 由它自己统计，测试据此断言「不该发生的调用没有发生」。
type fakeBackend struct {
	mu sync.Mutex

	acquireHook func(call int) (bool, error)
	renewHook   func(call int) (bool, error)
	releaseHook func(call int) (bool, error)

	acquireCalls int
	renewCalls   int
	releaseCalls int

	deletedKeys    []string
	lastKey        string
	lastOwner      string
	lastAcquireTTL time.Duration

	// renewed 每次续约成功投递一个信号，供测试用「通道 + 超时」等待后台行为。
	renewed chan struct{}
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{renewed: make(chan struct{}, 256)}
}

// 编译期确认替身满足租约后端接口
var _ Backend = (*fakeBackend)(nil)

func (b *fakeBackend) Acquire(_ context.Context, key, owner string, ttl time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.acquireCalls++
	b.lastKey, b.lastOwner, b.lastAcquireTTL = key, owner, ttl
	if b.acquireHook != nil {
		return b.acquireHook(b.acquireCalls)
	}
	return true, nil
}

func (b *fakeBackend) Renew(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.renewCalls++
	ok, err := true, error(nil)
	if b.renewHook != nil {
		ok, err = b.renewHook(b.renewCalls)
	}
	if ok && err == nil {
		signal(b.renewed)
	}
	return ok, err
}

func (b *fakeBackend) Release(_ context.Context, key, _ string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.releaseCalls++
	ok, err := true, error(nil)
	if b.releaseHook != nil {
		ok, err = b.releaseHook(b.releaseCalls)
	}
	// 只有真的删掉了才记账：释放被拒时不能留下「删除过」的痕迹
	if ok && err == nil {
		b.deletedKeys = append(b.deletedKeys, key)
	}
	return ok, err
}

// Get 只是为了让替身满足 Backend 接口：Manager 从不调用它。
func (b *fakeBackend) Get(_ context.Context, _ string) (string, bool, error) {
	return "", false, nil
}

// calls 返回三个方法的调用次数。
func (b *fakeBackend) calls() (acquire, renew, release int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acquireCalls, b.renewCalls, b.releaseCalls
}

// deletions 返回被后端真正删掉的键。调用方拿到的是副本，避免数据竞争。
func (b *fakeBackend) deletions() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.deletedKeys...)
}

// lastAcquire 返回最近一次抢占收到的 key / owner / ttl。
func (b *fakeBackend) lastAcquire() (key, owner string, ttl time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastKey, b.lastOwner, b.lastAcquireTTL
}

// signal 非阻塞地投递事件：后台协程绝不能因为测试还没来得及收而卡住。
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// logRecorder 记录 Logf 的输出。
type logRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (r *logRecorder) Logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
}

func (r *logRecorder) contains(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, msg := range r.msgs {
		if strings.Contains(msg, sub) {
			return true
		}
	}
	return false
}

func (r *logRecorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.msgs...)
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// testManager 构造测试用 Manager，并补上非 nil 的 Logf。
//
// 补齐是刻意的：生命周期事件一定会走到日志分支，nil Logf 会 panic，
// 那会让与日志无关的用例一起炸掉、掩盖真正要断言的行为。
func testManager(t *testing.T, opts Options) *Manager {
	t.Helper()

	if opts.Logf == nil {
		opts.Logf = noopLogf
	}
	manager, err := New(opts)
	if err != nil {
		t.Fatalf("构造 Manager 失败：%v", err)
	}
	return manager
}

// waitEvent 等待一次后台事件，超时即判失败。
//
// 用「通道 + 超时」而不是 time.Sleep：等待由事件驱动，
// 机器慢只会拉长真实耗时，不会把「还没发生」误判成「已经发生」。
func waitEvent(t *testing.T, ch <-chan struct{}) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("等待后台事件超时")
	}
}

// assertNoEvent 断言在给定的宽限期内没有新事件。
//
// 用于「Watch 退出后不再续约」这类只能靠「不再发生」来证明的行为，
// 宽限期取若干倍心跳，既短到不影响测试时长，又足够容纳一次在途的 tick。
func assertNoEvent(t *testing.T, ch <-chan struct{}, grace time.Duration) {
	t.Helper()

	select {
	case <-ch:
		t.Fatal("本不该再发生的事件发生了")
	case <-time.After(grace):
	}
}

// ---------------------------------------------------------------------------
// New
// ---------------------------------------------------------------------------

// TestNewRejectsNilBackend 守住构造期的空后端检查。
//
// 没有后端的 Manager 只能靠 panic 暴露问题，而装配期的错误应该是返回错误：
// 容器启动时能明确知道是配置缺失，而不是进程崩在第一个请求上。
func TestNewRejectsNilBackend(t *testing.T) {
	manager, err := New(Options{Logf: noopLogf})
	if err == nil {
		t.Fatal("Backend 为 nil 时 New 必须报错")
	}
	if manager != nil {
		t.Errorf("构造失败时不应返回 Manager，得到 %v", manager)
	}
	if !strings.Contains(err.Error(), "缺少存储后端") {
		t.Errorf("错误信息应当说明缺的是存储后端：%q", err.Error())
	}
}

// TestNewRejectsHeartbeatNotShorterThanTTL 守住「心跳必须严格小于租期」。
//
// 两者相等或心跳更大时，一次续约还没落地租约就已经到期，
// 续约会永远失败而持有者以为一切正常——这种配置必须在启动期就被挡掉，
// 所以报错信息要同时给出两个时长，调用方才知道该改哪一个。
func TestNewRejectsHeartbeatNotShorterThanTTL(t *testing.T) {
	cases := []struct {
		name      string
		ttl       time.Duration
		heartbeat time.Duration
	}{
		{"心跳等于租期", 50 * time.Millisecond, 50 * time.Millisecond},
		{"心跳大于租期", 50 * time.Millisecond, 120 * time.Millisecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, err := New(Options{
				Backend:   newFakeBackend(),
				TTL:       tc.ttl,
				Heartbeat: tc.heartbeat,
				Logf:      noopLogf,
			})
			if err == nil {
				t.Fatal("心跳不小于租期时必须报错")
			}
			if manager != nil {
				t.Errorf("构造失败时不应返回 Manager，得到 %v", manager)
			}
			message := err.Error()
			if !strings.Contains(message, tc.ttl.String()) || !strings.Contains(message, tc.heartbeat.String()) {
				t.Errorf("错误信息应同时点出租期(%v)与心跳(%v)：%q", tc.ttl, tc.heartbeat, message)
			}
		})
	}
}

// TestNewAppliesDefaults 守住零值选项的兜底。
//
// 调用方只给 Backend 是常态，兜底值一旦被改错（例如心跳等于租期，
// 或前缀丢了结尾冒号导致不同租约共用一个键空间），后果都要等到线上才发现。
func TestNewAppliesDefaults(t *testing.T) {
	manager := testManager(t, Options{Backend: newFakeBackend()})

	if got := manager.TTL(); got != DefaultTTL {
		t.Errorf("TTL() = %v，期望默认值 %v", got, DefaultTTL)
	}
	if got := manager.Heartbeat(); got != DefaultHeartbeat {
		t.Errorf("Heartbeat() = %v，期望默认值 %v", got, DefaultHeartbeat)
	}
	if got := manager.waitTimeout; got != DefaultWaitTimeout {
		t.Errorf("waitTimeout = %v，期望默认值 %v", got, DefaultWaitTimeout)
	}
	if got := manager.prefix; got != "crosspilot:lease:session:" {
		t.Errorf("prefix = %q，期望默认前缀 %q", got, "crosspilot:lease:session:")
	}

	// 默认时钟必须是墙上时钟：忘了兜底会在第一次 Now() 时 panic
	before := time.Now()
	now := manager.clock.Now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Errorf("默认时钟返回 %v，落在 [%v, %v] 之外", now, before, after)
	}

	// 零值构造也必须能正常抢占，并把默认前缀拼进键名
	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("默认配置下抢占失败：%v", err)
	}
	if want := "crosspilot:lease:session:session-1"; lease.Key != want {
		t.Errorf("Key = %q，期望 %q", lease.Key, want)
	}
}

// TestNewKeepsExplicitOptions 守住显式配置原样生效。
//
// 租期与心跳写错会让互斥缩短到无意义的窗口；前缀写错会让不同环境的
// 租约互相踩踏。这里逐项确认配置没有被兜底逻辑覆盖掉。
func TestNewKeepsExplicitOptions(t *testing.T) {
	clock := newFakeClock()
	backend := newFakeBackend()
	manager := testManager(t, Options{
		Backend:     backend,
		Prefix:      "自定义环境:租约:",
		TTL:         2 * time.Second,
		Heartbeat:   200 * time.Millisecond,
		WaitTimeout: 3 * time.Second,
		Clock:       clock,
	})

	if manager.TTL() != 2*time.Second {
		t.Errorf("TTL() = %v，期望 2s", manager.TTL())
	}
	if manager.Heartbeat() != 200*time.Millisecond {
		t.Errorf("Heartbeat() = %v，期望 200ms", manager.Heartbeat())
	}
	if manager.waitTimeout != 3*time.Second {
		t.Errorf("waitTimeout = %v，期望 3s", manager.waitTimeout)
	}
	if manager.clock != clock {
		t.Error("显式传入的 Clock 被替换掉了，测试将无法驱动时间")
	}

	lease, err := manager.Acquire(context.Background(), "s1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	if want := "自定义环境:租约:s1"; lease.Key != want {
		t.Errorf("Key = %q，期望 %q", lease.Key, want)
	}
	key, owner, ttl := backend.lastAcquire()
	if key != lease.Key {
		t.Errorf("后端收到的 key = %q，期望 %q", key, lease.Key)
	}
	if owner != lease.OwnerToken() {
		t.Errorf("后端收到的 owner = %q，期望 %q", owner, lease.OwnerToken())
	}
	// 租期必须原样下发：单位或数值写错会让键提前过期，互斥形同虚设
	if ttl != 2*time.Second {
		t.Errorf("后端收到的 ttl = %v，期望 2s", ttl)
	}
}

// ---------------------------------------------------------------------------
// Acquire
// ---------------------------------------------------------------------------

// TestAcquireGrantsLeaseAndExposesOwnerToken 守住抢占成功后的全部可观察结果。
//
// OwnerToken 会被写进持久层的 fence 条件，格式（32 位小写十六进制）
// 一旦跑偏，条件更新就永远匹配不上，表现为「谁都写不进去」。
func TestAcquireGrantsLeaseAndExposesOwnerToken(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{
		Backend:   backend,
		TTL:       time.Second,
		Heartbeat: 100 * time.Millisecond,
	})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占应当成功，得到错误：%v", err)
	}
	if lease == nil {
		t.Fatal("抢占成功时不应返回空租约")
	}
	if !lease.Valid() {
		t.Error("刚抢到的租约必须是有效的")
	}
	if !manager.Held("session-1") {
		t.Error("抢到之后 Held 必须为 true，否则调用方会以为没拿到执行权")
	}
	if manager.Held("session-2") {
		t.Error("没有抢占过的会话不能报告为已持有")
	}
	if lease.SessionID != "session-1" {
		t.Errorf("SessionID = %q，期望 session-1", lease.SessionID)
	}
	if want := "crosspilot:lease:session:session-1"; lease.Key != want {
		t.Errorf("Key = %q，期望 %q", lease.Key, want)
	}
	if manager.TTL() != time.Second {
		t.Errorf("TTL() = %v，期望 1s", manager.TTL())
	}
	if manager.Heartbeat() != 100*time.Millisecond {
		t.Errorf("Heartbeat() = %v，期望 100ms", manager.Heartbeat())
	}

	token := lease.OwnerToken()
	if len(token) != 32 {
		t.Errorf("OwnerToken 长度 = %d，期望 32（16 字节随机数的十六进制）", len(token))
	}
	for _, r := range token {
		isLowerHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
		if !isLowerHex {
			t.Fatalf("OwnerToken 含非小写十六进制字符：%q", token)
		}
	}
	if token != lease.Owner {
		t.Errorf("OwnerToken() = %q，与 Owner 字段 %q 不一致", token, lease.Owner)
	}

	// 随机性：两个会话的 owner 不能撞在一起，否则 fence 会把不同执行者当成同一个
	other, err := manager.Acquire(context.Background(), "session-2")
	if err != nil {
		t.Fatalf("第二个会话抢占失败：%v", err)
	}
	if other.OwnerToken() == token {
		t.Error("两次抢占生成了相同的持有者标识，随机性失效")
	}
}

// TestAcquireReusesHeldLease 守住重入复用。
//
// 同一个 Manager 在同一会话上重复抢占必须直接返回已持有的租约：
// 一个请求内部的两段逻辑各自抢占时，第二次去后端抢只会被自己挡住并等到超时。
func TestAcquireReusesHeldLease(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})
	ctx := context.Background()

	first, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("首次抢占失败：%v", err)
	}
	second, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("重复抢占失败：%v", err)
	}
	if first != second {
		t.Error("重复抢占应返回同一个租约指针，而不是新凭证")
	}
	if acquire, _, _ := backend.calls(); acquire != 1 {
		t.Errorf("重复抢占不应再访问后端，实际调用 %d 次", acquire)
	}

	// 不同会话必须各自抢占，不能被复用逻辑吞掉
	third, err := manager.Acquire(ctx, "session-2")
	if err != nil {
		t.Fatalf("第二个会话抢占失败：%v", err)
	}
	if third == first {
		t.Error("不同会话不能共用同一个租约")
	}
	if acquire, _, _ := backend.calls(); acquire != 2 {
		t.Errorf("第二个会话应当再抢占一次，实际累计 %d 次", acquire)
	}

	// 租约失效之后必须重新抢占：失效意味着执行权可能已经易主
	first.Invalidate()
	fresh, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("失效后重新抢占失败：%v", err)
	}
	if fresh == first {
		t.Error("已失效的租约不能被复用")
	}
	if acquire, _, _ := backend.calls(); acquire != 3 {
		t.Errorf("失效后必须重新访问后端，实际累计 %d 次", acquire)
	}
}

// TestAcquireRetriesUntilGranted 守住抢占失败后的轮询。
//
// 「没抢到」是正常的并发结果，不是错误：调用方应当被允许在原地等一小会儿。
// 一次重试的等待由实现里的 retryInterval（100ms）决定，测试无法绕过它，
// 因此这条用例的耗时下限就是一次轮询间隔。
func TestAcquireRetriesUntilGranted(t *testing.T) {
	backend := newFakeBackend()
	backend.acquireHook = func(call int) (bool, error) {
		return call >= 2, nil // 第一次被别人占着，第二次拿到
	}
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("重试后应当抢占成功，得到错误：%v", err)
	}
	if !lease.Valid() {
		t.Error("重试拿到的租约必须有效")
	}
	acquire, _, _ := backend.calls()
	if acquire != 2 {
		t.Errorf("后端抢占调用次数 = %d，期望 2（一次被拒 + 一次成功）", acquire)
	}
	if acquire <= 1 {
		t.Error("抢占失败后必须至少重试一次，不能一次就放弃")
	}
}

// TestAcquireTimesOutWhenBackendKeepsRefusing 守住等待上限。
//
// 一直抢不到必须明确失败：无限排队会把一次拥塞变成一次积压。
// 假时钟由替身后端每次轮询推进 20ms，等待上限设成 30ms，
// 于是「第二次轮询时已经超时」这一判定与真实调度无关。
func TestAcquireTimesOutWhenBackendKeepsRefusing(t *testing.T) {
	clock := newFakeClock()
	backend := newFakeBackend()
	backend.acquireHook = func(int) (bool, error) {
		clock.Advance(20 * time.Millisecond) // 每次轮询消耗一点时间
		return false, nil
	}
	manager := testManager(t, Options{
		Backend:     backend,
		Clock:       clock,
		TTL:         time.Second,
		Heartbeat:   100 * time.Millisecond,
		WaitTimeout: 30 * time.Millisecond,
	})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("等待超时应返回 ErrTimeout，得到 %v", err)
	}
	if lease != nil {
		t.Errorf("超时时不应返回租约，得到 %v", lease)
	}
	if manager.Held("session-1") {
		t.Error("超时后不能报告为已持有")
	}
	acquire, _, _ := backend.calls()
	if acquire <= 1 {
		t.Errorf("超时前应当轮询多次，实际只调用 %d 次", acquire)
	}
}

// TestAcquireReturnsContextErrorWhenCancelled 守住等待期间的取消响应。
//
// 调用方的 ctx 结束（用户断开、上层超时）时必须立刻返回 ctx 的错误，
// 而不是继续等满 WaitTimeout —— 后者会让一次取消拖住一个 goroutine。
func TestAcquireReturnsContextErrorWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := newFakeBackend()
	backend.acquireHook = func(int) (bool, error) {
		cancel() // 第一次抢占就遇到取消
		return false, nil
	}
	manager := testManager(t, Options{
		Backend:     backend,
		Clock:       newFakeClock(), // 假时钟不前进，超时分支永远不会先命中
		TTL:         time.Second,
		Heartbeat:   100 * time.Millisecond,
		WaitTimeout: time.Minute,
	})

	start := time.Now()
	lease, err := manager.Acquire(ctx, "session-1")
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx 取消后应返回 context.Canceled，得到 %v", err)
	}
	if lease != nil {
		t.Errorf("取消时不应返回租约，得到 %v", lease)
	}
	if elapsed > time.Second {
		t.Errorf("取消后耗时 %v，说明没有立刻响应 ctx", elapsed)
	}
}

// TestAcquireWrapsBackendError 守住后端故障的错误传递。
//
// 后端不可用与「没抢到」是两回事：前者要降级到单进程模式，后者只是稍后重试。
// 因此底层错误必须能被 errors.Is 认出来，而不是被换成一句笼统的失败。
func TestAcquireWrapsBackendError(t *testing.T) {
	backend := newFakeBackend()
	backend.acquireHook = func(int) (bool, error) { return false, errBackendDown }
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err == nil {
		t.Fatal("后端报错时抢占必须失败")
	}
	if !errors.Is(err, errBackendDown) {
		t.Errorf("底层错误应被包装保留，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "抢占") {
		t.Errorf("错误信息应说明是抢占失败：%q", err.Error())
	}
	if lease != nil {
		t.Errorf("失败时不应返回租约，得到 %v", lease)
	}
	if manager.Held("session-1") {
		t.Error("后端报错后不能报告为已持有")
	}
}

// 关于 newOwner 的错误分支（lease.go 里 `rand.Read` 失败后返回错误那一段）：
//
// 它在本仓库使用的 Go 版本上无法被覆盖，也无法被断言。Go 1.24 起
// crypto/rand.Read 遇到熵源故障会直接 fatal（见 go.dev/issue/66821），
// 进程当场退出，连 recover 都拦不住——实测替换 crypto/rand.Reader 后
// 得到的是 `fatal error: crypto/rand: failed to read random data`。
// 也就是说那个错误分支是防御性代码：留着无害，但永远不会返回。
// 待覆盖率为 98.4%，未覆盖的三条语句全部属于这类不可达分支。

// ---------------------------------------------------------------------------
// Renew
// ---------------------------------------------------------------------------

// TestRenewResetsStaleness 守住成功续约会刷新陈旧计时。
//
// staleFor 是 Watch 判定「本进程是否被长时间暂停」的依据。
// 续约成功却不刷新它，正常运行的持有者会在两个心跳后被自己作废。
func TestRenewResetsStaleness(t *testing.T) {
	clock := newFakeClock()
	backend := newFakeBackend()
	manager := testManager(t, Options{
		Backend:   backend,
		Clock:     clock,
		TTL:       time.Second,
		Heartbeat: 100 * time.Millisecond,
	})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}

	clock.Advance(3 * time.Second)
	if got := lease.staleFor(); got != 3*time.Second {
		t.Errorf("推进 3s 后 staleFor = %v，期望 3s", got)
	}

	ok, err := lease.Renew(context.Background())
	if err != nil {
		t.Fatalf("续约应当成功，得到错误：%v", err)
	}
	if !ok {
		t.Error("续约应当返回 ok")
	}
	if got := lease.staleFor(); got != 0 {
		t.Errorf("续约成功后 staleFor = %v，期望 0", got)
	}
	if !lease.Valid() {
		t.Error("续约成功不应影响有效性")
	}
	if _, renew, _ := backend.calls(); renew != 1 {
		t.Errorf("后端续约调用次数 = %d，期望 1", renew)
	}
}

// TestRenewReportsLostAndInvalidatesLease 守住「键已易主」的处理。
//
// 后端说「不再是你的」时，本地必须同时做到三件事：返回 ErrLost、
// 立刻作废租约、触发丢失回调。任何一步缺失，生产者协程都会继续往
// 已经不属于自己的会话里写。
func TestRenewReportsLostAndInvalidatesLease(t *testing.T) {
	backend := newFakeBackend()
	backend.renewHook = func(int) (bool, error) { return false, nil }
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})
	ctx := context.Background()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lost := make(chan struct{}, 4)
	lease.OnLost(func() { lost <- struct{}{} })

	ok, err := lease.Renew(ctx)
	if ok {
		t.Error("后端说键已易主时不能报告续约成功")
	}
	if !errors.Is(err, ErrLost) {
		t.Fatalf("应返回 ErrLost，得到 %v", err)
	}
	if lease.Valid() {
		t.Error("失去执行权后租约必须立刻失效")
	}
	if manager.Held("session-1") {
		t.Error("失去执行权后 Held 必须为 false")
	}
	waitEvent(t, lost)
	assertNoEvent(t, lost, 50*time.Millisecond) // 回调只能触发一次

	// 已失效的租约不该再打扰后端
	ok, err = lease.Renew(ctx)
	if ok || !errors.Is(err, ErrNotHeld) {
		t.Errorf("再次续约应得到 ErrNotHeld，得到 ok=%v err=%v", ok, err)
	}
	if _, renew, _ := backend.calls(); renew != 1 {
		t.Errorf("已失效的租约不应再访问后端，实际续约调用 %d 次", renew)
	}
}

// TestRenewWrapsBackendError 守住续约期间的传递性故障。
//
// 网络抖动导致的续约失败与「执行权真的丢了」必须区分：前者应原样上报，
// 由上层决定是否重试或降级，而不是被当成已经失去执行权。
func TestRenewWrapsBackendError(t *testing.T) {
	backend := newFakeBackend()
	backend.renewHook = func(int) (bool, error) { return false, errBackendDown }
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lost := make(chan struct{}, 1)
	lease.OnLost(func() { lost <- struct{}{} })

	ok, err := lease.Renew(context.Background())
	if ok {
		t.Error("后端报错时不能报告续约成功")
	}
	if !errors.Is(err, errBackendDown) {
		t.Errorf("底层错误应被包装保留，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "续约") {
		t.Errorf("错误信息应说明是续约失败：%q", err.Error())
	}
	if !lease.Valid() {
		t.Error("后端报错不等于执行权丢失，不应擅自作废本地租约")
	}
	assertNoEvent(t, lost, 20*time.Millisecond)
}

// TestRenewOnInvalidatedLeaseReturnsNotHeld 守住已作废租约的续约拒绝。
//
// 本地已经判定失去执行权之后再去续约，等于让一个已经被取消的生产者
// 继续参与互斥，因此必须直接拒绝，连后端都不该碰。
func TestRenewOnInvalidatedLeaseReturnsNotHeld(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lease.Invalidate()

	ok, err := lease.Renew(context.Background())
	if ok {
		t.Error("已作废的租约不能续约成功")
	}
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("应返回 ErrNotHeld，得到 %v", err)
	}
	if _, renew, _ := backend.calls(); renew != 0 {
		t.Errorf("已作废的租约不应访问后端，实际续约调用 %d 次", renew)
	}
}

// TestRenewAfterReleaseReturnsNotHeld 守住「归还之后不能再续约」。
//
// 释放之后再续约是一次无意义的调用，更糟的是它会打到后端并把一个
// 已经归还的键重新延长——如果那一刻别人刚抢到这个键，续约就会失败，
// 于是「已经正常结束」的这次执行被当成「失去执行权」再走一遍失败收尾。
// 因此释放必须同时让租约失效，续约直接返回 ErrNotHeld 且不碰后端。
func TestRenewAfterReleaseReturnsNotHeld(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})
	ctx := context.Background()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("释放失败：%v", err)
	}

	if _, renew, _ := backend.calls(); renew != 0 {
		t.Fatalf("释放前不应有续约调用，实际 %d 次", renew)
	}
	ok, err := lease.Renew(ctx)
	if !errors.Is(err, ErrNotHeld) {
		t.Fatalf("释放后续约应返回 ErrNotHeld，实际 ok=%v err=%v", ok, err)
	}
	if ok {
		t.Error("续约不应报告成功")
	}
	if _, renew, _ := backend.calls(); renew != 0 {
		t.Errorf("释放后的续约不应访问后端，实际调用 %d 次", renew)
	}
	if lease.Valid() {
		t.Error("释放后租约不应仍然有效")
	}
}

// ---------------------------------------------------------------------------
// Invalidate
// ---------------------------------------------------------------------------

// TestInvalidateMarksInvalidBeforeCallbacksRun 守住「先作废、后通知」的顺序。
//
// 这是整条链上最要紧的顺序：回调的职责是取消生产者协程，如果它运行时
// 观察到租约仍然有效，就可能基于错误判断再写一次。断言必须写在回调里面，
// 否则测的只是调用之后的最终状态。
func TestInvalidateMarksInvalidBeforeCallbacksRun(t *testing.T) {
	recorder := &logRecorder{}
	manager := testManager(t, Options{Backend: newFakeBackend(), Logf: recorder.Logf})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}

	var (
		validInsideCallback bool
		heldInsideCallback  bool
		callbackRan         bool
	)
	lease.OnLost(func() {
		callbackRan = true
		validInsideCallback = lease.Valid()
		heldInsideCallback = manager.Held("session-1")
	})

	lease.Invalidate()

	if !callbackRan {
		t.Fatal("Invalidate 必须触发 OnLost 回调")
	}
	if validInsideCallback {
		t.Error("回调运行时租约必须已经无效，否则回调会基于错误状态继续写")
	}
	if heldInsideCallback {
		t.Error("回调运行时 Held 必须已经是 false")
	}
	if lease.Valid() {
		t.Error("Invalidate 之后租约必须无效")
	}
	if manager.Held("session-1") {
		t.Error("Invalidate 之后 Held 必须为 false")
	}
	if !recorder.contains("租约已失效") {
		t.Errorf("Invalidate 应当记录一条日志，实际输出：%v", recorder.messages())
	}
}

// TestInvalidateRunsCallbacksOnceInRegistrationOrder 守住回调的数量与顺序。
//
// 回调做的是「取消生产者」这类不可重复的动作：跑两次会让收尾互相干扰，
// 顺序颠倒则可能先释放资源再通知取消。重复调用 Invalidate 也必须幂等。
func TestInvalidateRunsCallbacksOnceInRegistrationOrder(t *testing.T) {
	manager := testManager(t, Options{Backend: newFakeBackend()})
	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}

	var order []string
	lease.OnLost(func() { order = append(order, "first") })
	lease.OnLost(func() { order = append(order, "second") })
	lease.OnLost(func() { order = append(order, "third") })

	lease.Invalidate()
	lease.Invalidate()
	lease.Invalidate()

	want := []string{"first", "second", "third"}
	if len(order) != len(want) {
		t.Fatalf("回调执行次数 = %d，期望 %d（重复 Invalidate 不得重放）", len(order), len(want))
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("回调执行顺序 = %v，期望 %v", order, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Watch
// ---------------------------------------------------------------------------

// TestWatchRenewsWithFakeClock 守住后台续约本身。
//
// 假时钟在这里只由「续约成功」推动（每次 +1ms），因此 staleFor 永远
// 远小于 2*Heartbeat，无论真实调度慢到什么程度都不会误判为「进程被暂停」——
// 这正是用假时钟而不是真睡眠驱动时间的原因：断言与机器快慢无关。
func TestWatchRenewsWithFakeClock(t *testing.T) {
	clock := newFakeClock()
	backend := newFakeBackend()
	backend.renewHook = func(int) (bool, error) {
		clock.Advance(time.Millisecond)
		return true, nil
	}
	manager := testManager(t, Options{
		Backend:   backend,
		Clock:     clock,
		TTL:       time.Second,
		Heartbeat: 10 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lease.Watch(ctx)

	// 后台续约是异步的，用事件而不是睡眠来等它发生
	for i := 0; i < 3; i++ {
		waitEvent(t, backend.renewed)
	}
	if !lease.Valid() {
		t.Error("正常续约期间租约必须保持有效")
	}
	if _, renew, _ := backend.calls(); renew < 3 {
		t.Errorf("后端续约调用次数 = %d，期望至少 3", renew)
	}
	if got := lease.staleFor(); got > manager.Heartbeat() {
		t.Errorf("续约成功后 staleFor = %v，应当远小于心跳间隔 %v", got, manager.Heartbeat())
	}
}

// TestWatchInvalidatesWhenRenewalRejected 守住续约被拒后的自毁。
//
// 后台心跳发现键已易主时，必须先作废本地租约并触发回调，
// 然后自身退出——继续跑下去只会让一个已经失去执行权的进程反复打扰后端。
func TestWatchInvalidatesWhenRenewalRejected(t *testing.T) {
	backend := newFakeBackend()
	backend.renewHook = func(call int) (bool, error) {
		return call < 2, nil // 第二次续约时键已经是别人的
	}
	manager := testManager(t, Options{
		Backend:   backend,
		Clock:     newFakeClock(),
		TTL:       time.Second,
		Heartbeat: 5 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lost := make(chan struct{}, 4)
	lease.OnLost(func() { lost <- struct{}{} })
	lease.Watch(ctx)

	waitEvent(t, lost)
	if lease.Valid() {
		t.Error("续约被拒后租约必须失效")
	}
	if manager.Held("session-1") {
		t.Error("续约被拒后 Held 必须为 false")
	}

	// Watch 应当在作废后退出：等一个在途 tick 的宽限期，续约次数不再增长
	_, before, _ := backend.calls()
	time.Sleep(20 * time.Millisecond)
	_, after, _ := backend.calls()
	if after != before {
		t.Errorf("Watch 未能退出：续约次数从 %d 增长到 %d", before, after)
	}
}

// TestWatchSelfInvalidatesWhenStale 守住「长时间暂停」分支。
//
// 手法说明：Watch 的心跳由真实 time.Ticker 驱动，假时钟推不动它；
// 而 staleFor 读的是 Clock。于是这里用假时钟把「距上次成功续约的时长」
// 一次性推到 1s，再让 5ms 的真实 ticker 去触发那次检查——两者配合后
// 既不需要真实等待 2*Heartbeat，也不会因为调度抖动而变红。
// 同时断言「先检查陈旧、再续约」的顺序：判定为陈旧时不应先续一次约。
func TestWatchSelfInvalidatesWhenStale(t *testing.T) {
	clock := newFakeClock()
	backend := newFakeBackend()
	manager := testManager(t, Options{
		Backend:   backend,
		Clock:     clock,
		TTL:       time.Second,
		Heartbeat: 5 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	// 模拟「进程被暂停」：远超过 2*Heartbeat 没有成功续约
	clock.Advance(time.Second)

	lost := make(chan struct{}, 4)
	lease.OnLost(func() { lost <- struct{}{} })
	lease.Watch(ctx)

	waitEvent(t, lost)
	if lease.Valid() {
		t.Error("超过两个心跳没有成功续约时，租约必须自我作废")
	}
	if manager.Held("session-1") {
		t.Error("自我作废后 Held 必须为 false")
	}
	if _, renew, _ := backend.calls(); renew != 0 {
		t.Errorf("判定为陈旧时不应先续约，实际续约 %d 次", renew)
	}
}

// TestWatchExitsWhenLeaseAlreadyInvalid 守住「启动时租约已失效」的短路。
//
// 已经失效的租约不该再启动续约：那等于让一个已被取消的持有者
// 继续参与互斥。这里断言后端完全没有被触碰。
func TestWatchExitsWhenLeaseAlreadyInvalid(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{
		Backend:   backend,
		Clock:     newFakeClock(),
		TTL:       time.Second,
		Heartbeat: 5 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lease.Invalidate()
	lease.Watch(ctx)

	// 等若干个心跳：若 Watch 没有短路退出，续约信号一定会出现
	select {
	case <-backend.renewed:
		t.Fatal("已失效的租约不应启动后台续约")
	case <-time.After(40 * time.Millisecond):
	}
	if _, renew, _ := backend.calls(); renew != 0 {
		t.Errorf("已失效的租约不应访问后端，实际续约 %d 次", renew)
	}
}

// TestWatchStopsAfterContextCancelled 守住心跳随 ctx 结束。
//
// 请求结束后的 ctx 取消必须让后台续约停下来，否则一个已经没人关心的
// 会话会一直被续期，把真正的新持有者挡在门外。
func TestWatchStopsAfterContextCancelled(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{
		Backend:   backend,
		Clock:     newFakeClock(),
		TTL:       time.Second,
		Heartbeat: 5 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lease.Watch(ctx)
	waitEvent(t, backend.renewed)
	waitEvent(t, backend.renewed)

	cancel()
	// 取消后先给一个宽限期让在途的 tick 落地，再看续约计数是否停住
	time.Sleep(25 * time.Millisecond)
	_, before, _ := backend.calls()
	time.Sleep(25 * time.Millisecond)
	_, after, _ := backend.calls()

	if after != before {
		t.Errorf("ctx 取消后仍在续约：续约次数从 %d 增长到 %d", before, after)
	}
	if !lease.Valid() {
		t.Error("ctx 取消只停心跳，不应把租约标为失效——是否归还执行权由调用方决定")
	}
}

// TestWatchStopsAfterRelease 守住释放会停掉后台心跳。
//
// Release 之后本进程不该再续期：继续续约等于把已经交还的执行权又攥在手里。
func TestWatchStopsAfterRelease(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{
		Backend:   backend,
		Clock:     newFakeClock(),
		TTL:       time.Second,
		Heartbeat: 5 * time.Millisecond,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lease.Watch(ctx)
	waitEvent(t, backend.renewed)

	if err := lease.Release(ctx); err != nil {
		t.Fatalf("释放失败：%v", err)
	}
	// stopWatch 关闭后最多还有一次在途 tick，先让它落地再比较
	time.Sleep(25 * time.Millisecond)
	_, before, _ := backend.calls()
	time.Sleep(25 * time.Millisecond)
	_, after, _ := backend.calls()

	if after != before {
		t.Errorf("Release 之后仍在续约：续约次数从 %d 增长到 %d", before, after)
	}
	if !lease.watchClosed {
		t.Error("Release 应当关闭后台续约的停止信号")
	}
	if !lease.released {
		t.Error("Release 应当把租约标记为已归还")
	}
}

// ---------------------------------------------------------------------------
// Release
// ---------------------------------------------------------------------------

// TestReleaseReturnsNilAndDeletesOwnKey 守住正常归还。
//
// 持有者主动归还时键必须被删掉：留着它只能等 TTL 自然过期，
// 这段时间里新请求会被无谓地挡在门外。
func TestReleaseReturnsNilAndDeletesOwnKey(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("归还执行权应当成功，得到错误：%v", err)
	}
	if manager.Held("session-1") {
		t.Error("归还之后 Held 必须为 false")
	}
	if _, _, release := backend.calls(); release != 1 {
		t.Errorf("后端释放调用次数 = %d，期望 1", release)
	}
	deleted := backend.deletions()
	if len(deleted) != 1 || deleted[0] != lease.Key {
		t.Errorf("后端删除的键 = %v，期望只删自己的 %q", deleted, lease.Key)
	}
}

// TestReleaseIsIdempotent 守住重复归还。
//
// 收尾路径常常被 defer 与显式调用各执行一次：第二次必须是无害的空操作，
// 而不是再删一次键——那可能在别人已经接手之后把新持有者的键删掉。
func TestReleaseIsIdempotent(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})
	ctx := context.Background()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	for i := 0; i < 3; i++ {
		if err := lease.Release(ctx); err != nil {
			t.Fatalf("第 %d 次归还不应报错：%v", i+1, err)
		}
	}
	if _, _, release := backend.calls(); release != 1 {
		t.Errorf("后端释放调用次数 = %d，期望 1（后续调用必须是空操作）", release)
	}
	if deleted := backend.deletions(); len(deleted) != 1 {
		t.Errorf("删除次数 = %d，期望 1", len(deleted))
	}
}

// TestReleaseWrapsBackendError 守住释放失败的上报。
//
// 释放失败不是业务失败（键会自然过期），但也不能假装成功：
// 调用方需要知道这次归还没有落地，同时日志里要留下线索。
func TestReleaseWrapsBackendError(t *testing.T) {
	recorder := &logRecorder{}
	backend := newFakeBackend()
	backend.releaseHook = func(int) (bool, error) { return false, errBackendDown }
	manager := testManager(t, Options{
		Backend:   backend,
		Logf:      recorder.Logf,
		TTL:       time.Second,
		Heartbeat: 100 * time.Millisecond,
	})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	err = lease.Release(context.Background())
	if err == nil {
		t.Fatal("后端报错时释放必须返回错误")
	}
	if !errors.Is(err, errBackendDown) {
		t.Errorf("底层错误应被包装保留，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "释放") {
		t.Errorf("错误信息应说明是释放失败：%q", err.Error())
	}
	if !recorder.contains("租约释放失败") {
		t.Errorf("释放失败必须留下日志，实际输出：%v", recorder.messages())
	}
	if deleted := backend.deletions(); len(deleted) != 0 {
		t.Errorf("释放失败时不应删除任何键，实际删除 %v", deleted)
	}
	if manager.Held("session-1") {
		t.Error("释放之后本进程不再持有执行权，Held 必须为 false")
	}
}

// TestReleaseWhenKeyStolenLogsWithoutDeleting 守住「不删别人的键」。
//
// 键在租约过期后被别人接管时，释放若还去 DEL，删掉的是新持有者的键，
// 互斥会被彻底打穿。后端因此返回「没删成」，这属于正常结果而非错误。
func TestReleaseWhenKeyStolenLogsWithoutDeleting(t *testing.T) {
	recorder := &logRecorder{}
	backend := newFakeBackend()
	backend.releaseHook = func(int) (bool, error) { return false, nil } // 键已是别人的
	manager := testManager(t, Options{
		Backend:   backend,
		Logf:      recorder.Logf,
		TTL:       time.Second,
		Heartbeat: 100 * time.Millisecond,
	})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	if err := lease.Release(context.Background()); err != nil {
		t.Errorf("「键已被接管」不是错误，键会自然过期：%v", err)
	}
	if deleted := backend.deletions(); len(deleted) != 0 {
		t.Errorf("绝不能删除新持有者的键，实际删除 %v", deleted)
	}
	if !recorder.contains("已被他人接管") {
		t.Errorf("键被接管必须留下日志，实际输出：%v", recorder.messages())
	}
}

// TestReleaseOnInvalidatedLeaseSkipsBackend 守住失效后释放的空操作。
//
// 租约已经失效说明键很可能已经易主，此时再去后端释放就是在删别人的键。
// 本地状态已经足够判定，因此必须直接返回，连后端都不碰。
func TestReleaseOnInvalidatedLeaseSkipsBackend(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})

	lease, err := manager.Acquire(context.Background(), "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lease.Invalidate()

	if err := lease.Release(context.Background()); err != nil {
		t.Errorf("已失效的租约释放应当直接成功：%v", err)
	}
	if _, _, release := backend.calls(); release != 0 {
		t.Errorf("已失效的租约不应访问后端，实际释放调用 %d 次", release)
	}
	if deleted := backend.deletions(); len(deleted) != 0 {
		t.Errorf("已失效的租约不应删除任何键，实际删除 %v", deleted)
	}
}

// TestReleaseKeepsNewOwnersLeaseHeld 守住旧租约不会顶掉新持有者。
//
// 场景：旧租约失效（例如进程暂停被判陈旧）后，同一进程立刻重新抢占成功。
// 此时旧租约的 Release/forget 绝不能把新租约从持有表里抹掉，
// 否则同一进程内的重入检查会失效，进而重复抢键。
func TestReleaseKeepsNewOwnersLeaseHeld(t *testing.T) {
	backend := newFakeBackend()
	manager := testManager(t, Options{Backend: backend, TTL: time.Second, Heartbeat: 100 * time.Millisecond})
	ctx := context.Background()

	old, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	old.Invalidate()

	fresh, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("重新抢占失败：%v", err)
	}
	if fresh == old {
		t.Fatal("旧租约已失效，重新抢占必须得到新租约")
	}

	if err := old.Release(ctx); err != nil {
		t.Fatalf("旧租约释放不应报错：%v", err)
	}
	if !manager.Held("session-1") {
		t.Error("旧租约的释放把新租约从持有表里抹掉了")
	}
	if _, _, release := backend.calls(); release != 0 {
		t.Errorf("旧租约已失效，释放不应访问后端，实际调用 %d 次", release)
	}
}

// TestReleaseAfterWatchExitDoesNotDoubleClose 守住停止通道只关一次。
//
// Release 会关闭 stopWatch 通知后台心跳退出；若没有 watchClosed 守卫，
// 「Watch 已自行退出后再 Release」或「重复 Release」都会二次关闭通道，
// 直接 panic 掉整个进程。
func TestReleaseAfterWatchExitDoesNotDoubleClose(t *testing.T) {
	// 场景一：ctx 取消让 Watch 先退出，之后再 Release
	backend := newFakeBackend()
	manager := testManager(t, Options{
		Backend:   backend,
		Clock:     newFakeClock(),
		TTL:       time.Second,
		Heartbeat: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lease, err := manager.Acquire(ctx, "session-1")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lease.Watch(ctx)
	waitEvent(t, backend.renewed)
	cancel()
	time.Sleep(20 * time.Millisecond) // 让 Watch 从 ctx.Done 退出
	if err := lease.Release(ctx); err != nil {
		t.Errorf("Watch 退出后释放应当成功：%v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Errorf("重复释放应当成功：%v", err)
	}

	// 场景二：Watch 还在跑，Release 先关停止通道，Watch 从它退出后再 Release
	backend2 := newFakeBackend()
	manager2 := testManager(t, Options{
		Backend:   backend2,
		Clock:     newFakeClock(),
		TTL:       time.Second,
		Heartbeat: 5 * time.Millisecond,
	})
	ctx2 := context.Background()

	lease2, err := manager2.Acquire(ctx2, "session-2")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lease2.Watch(ctx2)
	waitEvent(t, backend2.renewed)
	if err := lease2.Release(ctx2); err != nil {
		t.Errorf("释放正在续约的租约应当成功：%v", err)
	}
	if err := lease2.Release(ctx2); err != nil {
		t.Errorf("重复释放应当成功：%v", err)
	}
	select {
	case <-lease2.stopWatch:
	default:
		t.Error("Release 应当关闭 stopWatch，否则后台心跳会一直跑下去")
	}
}

// ---------------------------------------------------------------------------
// 日志
// ---------------------------------------------------------------------------

// TestLogfRecordsLifecycleEvents 守住日志覆盖的四类事件。
//
// 租约是排障时最难复现的一类问题（谁在什么时候拿走了执行权），
// 取得、失效、释放失败、键被接管这四条线索缺一条，
// 事后就只能靠猜。
func TestLogfRecordsLifecycleEvents(t *testing.T) {
	recorder := &logRecorder{}
	backend := newFakeBackend()
	manager := testManager(t, Options{
		Backend:   backend,
		Logf:      recorder.Logf,
		TTL:       time.Second,
		Heartbeat: 100 * time.Millisecond,
	})
	ctx := context.Background()

	// 取得
	acquired, err := manager.Acquire(ctx, "session-acquire")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	if !recorder.contains("租约已取得") || !recorder.contains("session-acquire") {
		t.Errorf("抢占成功必须记录会话号，实际输出：%v", recorder.messages())
	}
	if !recorder.contains(acquired.OwnerToken()) {
		t.Errorf("抢占日志应带上持有者标识，实际输出：%v", recorder.messages())
	}

	// 失效
	acquired.Invalidate()
	if !recorder.contains("租约已失效") {
		t.Errorf("失效必须记录日志，实际输出：%v", recorder.messages())
	}

	// 释放失败
	backend.releaseHook = func(int) (bool, error) { return false, errBackendDown }
	failed, err := manager.Acquire(ctx, "session-release-error")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	if err := failed.Release(ctx); err == nil {
		t.Fatal("后端报错时释放必须返回错误")
	}
	if !recorder.contains("租约释放失败") {
		t.Errorf("释放失败必须记录日志，实际输出：%v", recorder.messages())
	}

	// 键被他人接管
	backend.releaseHook = func(int) (bool, error) { return false, nil }
	stolen, err := manager.Acquire(ctx, "session-stolen")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	if err := stolen.Release(ctx); err != nil {
		t.Fatalf("键被接管时释放不应报错：%v", err)
	}
	if !recorder.contains("已被他人接管") {
		t.Errorf("键被接管必须记录日志，实际输出：%v", recorder.messages())
	}
}

// TestNilLogfIsSilent 守住「日志是可选的」。
//
// Options.Logf 的注释写着「为 nil 时静默」。若 New 不把它兜底成空实现，
// 而 Acquire/Invalidate/Release 都直接调用它，那么任何没传日志的装配点
// 都会在第一次抢占成功时 panic——一个可选参数让进程崩溃，
// 这不是调用方的错，是接口没有兑现自己的承诺。
func TestNilLogfIsSilent(t *testing.T) {
	manager, err := New(Options{Backend: newFakeBackend()}) // 刻意不传 Logf
	if err != nil {
		t.Fatalf("Logf 为 nil 不应导致构造失败：%v", err)
	}

	if manager.Held("session-1") {
		t.Error("没有抢占过的会话不应报告为已持有")
	}

	// 走到日志分支的所有路径都必须安静地过去：抢占、失效、释放
	lease, err := manager.Acquire(context.Background(), "session-2")
	if err != nil {
		t.Fatalf("抢占失败：%v", err)
	}
	lease.Invalidate()
	if err := lease.Release(context.Background()); err != nil {
		t.Fatalf("释放失败：%v", err)
	}
}
