package observability

import (
	"context"
	"testing"
	"time"
)

// TestClose_落库队列中积压的记录 是一条防数据丢失的回归。
//
// Emit 是同步把记录塞进 channel 就返回的，调用方（Bridge）在它返回后就认为
// 「这条事件已经发出去了」。而 worker 循环的 select 在 e.closed 与 e.queue
// 同时就绪时是**随机**选的：若关闭时选中 closed 分支而不把 channel 里剩下的
// 记录收走，这些事件永远不会落库，且没有任何计数与日志——看起来一切正常。
//
// 默认 BatchSize=64 / FlushEvery=100ms，正常关闭时积压几十条是常态。
func TestClose_落库队列中积压的记录(t *testing.T) {
	sink := NewMemorySink()
	// BatchSize 取得远大于事件数：强制让记录停在 channel 里而不是被攒进 batch，
	// 这正是「关闭时队列有积压」的形态。
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  64,
		BatchSize:  1000,
		FlushEvery: time.Hour, // 只靠 Close 触发，避免 ticker 先把它们写掉
		Redactor:   noopRedactor{},
	})
	emitter.Start(context.Background())

	const n = 20
	for i := int64(0); i < n; i++ {
		ev := makeEvent(i)
		ev.Seq = i
		emitter.Emit(ev)
	}
	if got := emitter.QueueDepth(); got != n {
		t.Fatalf("队列深度 = %d，期望 %d（记录应还停在 channel 里）", got, n)
	}

	if err := emitter.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if got := len(sink.Records()); got != n {
		t.Errorf("关闭后落库 %d 条，期望 %d 条——队列积压被静默丢弃了", got, n)
	}
}

// TestEmitter_CreatedAt是Unix秒 断言写库时间戳的单位。
//
// SinkRecord.CreatedAt 的契约是 **Unix 秒**，pg 侧按 time.Unix(sec, 0) 还原成
// TIMESTAMPTZ。传 UnixNano 会让还原出的年份超出 int64 微秒能表达的范围，
// 整批写入失败，而失败又被计入 dropped 静默吞掉——表里一行都没有，日志里
// 只有一条 dropped 计数，很难联想到是单位写错。
func TestEmitter_CreatedAt是Unix秒(t *testing.T) {
	sink := NewMemorySink()
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize: 8, BatchSize: 1, FlushEvery: 10 * time.Millisecond,
		Redactor: noopRedactor{},
	})
	emitter.Start(context.Background())
	defer func() { _ = emitter.Close() }()

	emitter.Emit(makeEvent(0))

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(sink.Records()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	recs := sink.Records()
	if len(recs) == 0 {
		t.Fatal("没有记录落库")
	}

	got := recs[0].CreatedAt
	// 秒级时间戳在 2001–2286 之间（1e9 ~ 1e10）。纳秒会大 9 个数量级。
	if got < 1_000_000_000 || got > 10_000_000_000 {
		t.Fatalf("CreatedAt = %d，不像 Unix 秒（纳秒会是 %d 量级）", got, got/1_000_000_000)
	}
}

// TestStart_在goroutine启动前登记 断言 wg.Add 先于 goroutine 启动。
//
// 若 Add 留在 Run 内部，Close 可能在 goroutine 被调度起来之前就走到 wg.Wait()，
// Wait 看到计数为 0 直接返回，随后 sink.Close() 与 worker 的 flush 并发——
// 最后一批要么写进已关闭的 sink，要么在进程退出时丢掉。
//
// 这条断言是「Start 之后立刻 Close，取消计数必须真正被等到」：用一个会记录
// 关闭顺序的 sink 来观察 sink.Close 是否发生在 worker 退出之后。
func TestStart_在goroutine启动前登记(t *testing.T) {
	sink := &orderTrackingSink{}
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize: 4, BatchSize: 1, FlushEvery: time.Hour,
		Redactor: noopRedactor{},
	})
	// 不等待、不做任何事，立刻关闭：这正是竞态窗口最小的时刻。
	emitter.Start(context.Background())
	emitter.Emit(makeEvent(0))
	if err := emitter.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if !sink.appendedBeforeClose {
		t.Error("Close 返回时最后一批还没写库——wg 没有等到 worker 退出")
	}
}

// orderTrackingSink 记录「关闭时是否已经收到过写入」。
type orderTrackingSink struct {
	appendedBeforeClose bool
	closed              bool
}

func (s *orderTrackingSink) Append(_ context.Context, batch []SinkRecord) error {
	if s.closed {
		panic("Append 发生在 Close 之后")
	}
	if len(batch) > 0 {
		s.appendedBeforeClose = true
	}
	return nil
}

func (s *orderTrackingSink) Close() error { s.closed = true; return nil }
