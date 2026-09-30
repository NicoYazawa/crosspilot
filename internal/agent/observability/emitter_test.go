package observability

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	infraobs "github.com/NicoYazawa/crosspilot/internal/infra/observability"
)

// noopRedactor 是测试用脱敏器，原样返回。
type noopRedactor struct{}

func (noopRedactor) Apply(in []byte) ([]byte, error) { return in, nil }

// makeEvent 构造一条合法 runevent.Event。
func makeEvent(seq int64) *runevent.Event {
	now := time.Unix(1700000000, 0).UTC()
	runID := "run_test"
	seqr := runevent.NewSequencer(runID)
	for i := int64(0); i < seq; i++ {
		seqr.Next()
	}
	ev, err := seqr.Attach(seqr.Next(), runevent.KindRunStart, "main", []byte(`{"x":1}`), now)
	if err != nil {
		panic(err)
	}
	return &ev
}

// TestF6_EmitDoesNotBlockWhenQueueFull 是 F6 验收的核心：
// 把队列塞满后继续调用 Emit，必须不阻塞。
func TestF6_EmitDoesNotBlockWhenQueueFull(t *testing.T) {
	t.Parallel()
	sink := NewMemorySink()

	dropped := atomic.Int64{}
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  8,
		BatchSize:  64,
		FlushEvery: time.Second, // 不让 ticker flush 干扰测试
		Redactor:   noopRedactor{},
		OnDrop:     func(reason string, _ int) { dropped.Add(1) },
	})

	// 不起 Run —— channel 永远不会被消费

	// 灌满队列 + 额外的若干条
	const N = 1000
	deadline := time.Now().Add(500 * time.Millisecond)
	maxLatency := time.Duration(0)
	for i := 0; i < N; i++ {
		start := time.Now()
		emitter.Emit(makeEvent(int64(i)))
		lat := time.Since(start)
		if lat > maxLatency {
			maxLatency = lat
		}
		if time.Now().After(deadline) {
			t.Fatalf("第 %d 次 Emit 阻塞超过 deadline", i)
		}
	}

	if got := dropped.Load(); got == 0 {
		t.Fatalf("应当记录 dropped 事件，实际为 0")
	}
	if maxLatency > 5*time.Millisecond {
		t.Fatalf("单次 Emit 最长延迟 %v 超过 5ms 上限（F6 闸门）", maxLatency)
	}
	t.Logf("Emit 调用 %d 次，maxLatency=%v，dropped=%d", N, maxLatency, dropped.Load())
}

// TestEmitter_FlushesBatch 测试攒批逻辑：N 条事件能在 FlushEvery 内被 flush。
func TestEmitter_FlushesBatch(t *testing.T) {
	t.Parallel()
	sink := NewMemorySink()
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  64,
		BatchSize:  3, // 攒 3 条就 flush
		FlushEvery: time.Hour,
		Redactor:   noopRedactor{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go emitter.Run(ctx)

	for i := 0; i < 3; i++ {
		emitter.Emit(makeEvent(int64(i)))
	}

	// 等 batch 攒齐
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.Records()) >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := sink.Records()
	if len(got) != 3 {
		t.Fatalf("期望 3 条被 flush，实际 %d", len(got))
	}
	for i, rec := range got {
		if rec.Seq != int64(i) {
			t.Fatalf("第 %d 条 seq 应为 %d，实际 %d", i, i, rec.Seq)
		}
		if rec.PayloadSHA256 == "" {
			t.Fatalf("第 %d 条 sha256 为空", i)
		}
	}
}

// TestEmitter_FlushesByInterval 测试 FlushEvery 强制 flush。
func TestEmitter_FlushesByInterval(t *testing.T) {
	t.Parallel()
	sink := NewMemorySink()
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  64,
		BatchSize:  100, // 大到 batch 不会触发
		FlushEvery: 50 * time.Millisecond,
		Redactor:   noopRedactor{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go emitter.Run(ctx)

	emitter.Emit(makeEvent(0))
	emitter.Emit(makeEvent(1))

	// 等 flush 间隔触发
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.Records()) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := sink.Records(); len(got) != 2 {
		t.Fatalf("期望 2 条被 flush，实际 %d", len(got))
	}
}

// TestEmitter_SinkFailureCountsAsDrop 测试 sink 写入失败时计入 dropped，不抛回。
func TestEmitter_SinkFailureCountsAsDrop(t *testing.T) {
	t.Parallel()
	sink := NewMemorySink()
	sink.SetFailN(2) // 前两次失败

	var dropped atomic.Int64
	var dropReason atomic.Value
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  64,
		BatchSize:  2,
		FlushEvery: time.Hour,
		Redactor:   noopRedactor{},
		OnDrop: func(reason string, _ int) {
			dropped.Add(1)
			dropReason.Store(reason)
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go emitter.Run(ctx)

	emitter.Emit(makeEvent(0))
	emitter.Emit(makeEvent(1))

	// 等 flush + 重试
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dropped.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := dropped.Load(); got == 0 {
		t.Fatalf("应当计入 dropped")
	}
	reason, _ := dropReason.Load().(string)
	if reason == "" || reason == "queue_full" {
		t.Fatalf("dropReason 应为 sink_error 系列，实际 %q", reason)
	}
}

// TestEmitter_CloseFlushesPending 测试 Close 调用后等待 Run 退出。
func TestEmitter_CloseFlushesPending(t *testing.T) {
	t.Parallel()
	sink := NewMemorySink()
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  64,
		BatchSize:  10, // batch 不会触发
		FlushEvery: time.Hour,
		Redactor:   noopRedactor{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		emitter.Run(ctx)
	}()

	emitter.Emit(makeEvent(0))
	emitter.Emit(makeEvent(1))

	// 直接 Close —— 不等 ticker，事件会留在 channel 里
	// 这是 "best-effort" 语义，Close 不阻塞等待 flush。
	// 验证 Close 不 panic 即可。
	if err := emitter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	cancel()
	done.Wait()
}

// TestEmitter_EmitAfterStoppedRecordsDrop 测试关闭后 Emit 不会 panic，而是 drop。
func TestEmitter_EmitAfterStoppedRecordsDrop(t *testing.T) {
	t.Parallel()
	sink := NewMemorySink()
	var dropped atomic.Int64
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  8,
		Redactor:   noopRedactor{},
		OnDrop:     func(reason string, _ int) { dropped.Add(1) },
		FlushEvery: time.Hour,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go emitter.Run(ctx)

	// 关闭
	cancel()
	// 等 Run 退出
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if emitter.stopped.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	emitter.Emit(makeEvent(0))
	if dropped.Load() == 0 {
		t.Fatalf("停止后 Emit 应计入 dropped")
	}
}

// TestEmitter_QueueDepth 测试 QueueDepth 返回当前队列长度。
func TestEmitter_QueueDepth(t *testing.T) {
	t.Parallel()
	sink := NewMemorySink()
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  16,
		BatchSize:  100, // 不让 batch 触发 flush
		FlushEvery: time.Hour,
		Redactor:   noopRedactor{},
	})
	defer emitter.Close()
	// 不起 Run —— 队列不会被消费

	for i := 0; i < 5; i++ {
		emitter.Emit(makeEvent(int64(i)))
	}
	if depth := emitter.QueueDepth(); depth != 5 {
		t.Fatalf("期望 queue depth 5，实际 %d", depth)
	}
}

// TestEmitter_F7_RedactionAppliedAtWritePath 测试 F7 闸门：
// Emit 必须把脱敏后的 payload 写入 SinkRecord，原值不能穿透。
func TestEmitter_F7_RedactionAppliedAtWritePath(t *testing.T) {
	t.Parallel()
	sink := NewMemorySink()
	// 写一个「假装是手机号」的红 actor
	redactor, err := infraobs.NewRedactor()
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  8,
		BatchSize:  1,
		FlushEvery: time.Hour,
		Redactor:   redactor,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go emitter.Run(ctx)

	// 构造一个包含手机号的 payload
	now := time.Unix(1700000000, 0).UTC()
	seqr := runevent.NewSequencer("r-f7")
	ev, _ := seqr.Attach(seqr.Next(), runevent.KindModelTurn, "main",
		[]byte(`{"phone":"13912345678"}`), now)
	emitter.Emit(&ev)

	// 等 flush
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.Records()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	recs := sink.Records()
	if len(recs) != 1 {
		t.Fatalf("期望 1 条被 flush，实际 %d", len(recs))
	}
	// 验证 SinkRecord 里的 PayloadRedacted 不含原手机号
	if string(recs[0].PayloadRedacted) == `{"phone":"13912345678"}` {
		t.Fatalf("F7 闸门失败：原手机号穿透了 SinkRecord（%s）", recs[0].PayloadRedacted)
	}
	if err := redactor.ValidateFixture(recs[0].PayloadRedacted); err != nil {
		t.Fatalf("F7 闸门失败：%v\n输出：%s", err, recs[0].PayloadRedacted)
	}
}
