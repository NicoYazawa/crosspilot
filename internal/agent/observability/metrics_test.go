package observability

import (
	"context"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// TestMetrics_DefaultIsNoOp 测试默认 metrics 是 no-op，
// 即不调 NewMetrics 时 Emitter 仍能正常工作。
func TestMetrics_DefaultIsNoOp(t *testing.T) {
	t.Parallel()
	sink := NewMemorySink()
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  8,
		Redactor:   noopRedactor{},
		FlushEvery: time.Hour,
	}) // 不传 Metrics

	emitter.Emit(makeEvent(0))
	emitter.Emit(makeEvent(1))

	// 两次 Emit 不应 panic（默认 metrics 是 no-op）
	// 直接调 Snapshot 也不应崩溃
	snap := emitter.cfg.Metrics.Snapshot()
	if snap.EmitTotal != 0 && snap.EmitTotal != 2 {
		t.Logf("默认 metrics 不计数（值 %d）", snap.EmitTotal)
	}
}

// TestMetrics_RecordsEmitAndDropped 测试显式 metrics 记录 Emit 与 dropped。
func TestMetrics_RecordsEmitAndDropped(t *testing.T) {
	t.Parallel()
	metrics := NewMetrics(uniqueMetricName("records"))
	sink := NewMemorySink()
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  4,
		BatchSize:  64,
		FlushEvery: time.Hour,
		Redactor:   noopRedactor{},
		Metrics:    metrics,
	})

	// 5 次 Emit，前 4 进去，第 5 触发 queue_full dropped
	for i := 0; i < 5; i++ {
		emitter.Emit(makeEvent(int64(i)))
	}

	snap := metrics.Snapshot()
	if snap.EmitTotal != 5 {
		t.Errorf("EmitTotal 应为 5，实际 %d", snap.EmitTotal)
	}
	if snap.DroppedTotal != 1 {
		t.Errorf("DroppedTotal 应为 1，实际 %d", snap.DroppedTotal)
	}
}

// TestMetrics_NilSafe 测试所有方法对 nil receiver 安全。
func TestMetrics_NilSafe(t *testing.T) {
	t.Parallel()
	var m *Metrics
	m.AddEmit(1)         // 不应 panic
	m.AddDropped(1)      // 不应 panic
	m.AddEmitErrors(1)   // 不应 panic
	m.AddRedactErrors(1) // 不应 panic
	m.SetQueueDepth(5)   // 不应 panic
	snap := m.Snapshot()
	if snap != (MetricsSnapshot{}) {
		t.Errorf("nil Metrics.Snapshot 应返回零值")
	}
}

// TestMetrics_QueueDepthUpdates 测试 QueueDepth 在 Emit 成功后更新。
func TestMetrics_QueueDepthUpdates(t *testing.T) {
	t.Parallel()
	metrics := NewMetrics(uniqueMetricName("qdepth"))
	sink := NewMemorySink()
	emitter := NewEmitter(sink, EmitterConfig{
		QueueSize:  16,
		BatchSize:  100,
		FlushEvery: time.Hour,
		Redactor:   noopRedactor{},
		Metrics:    metrics,
	})
	defer func() { _ = emitter.Close() }()

	// 不起 Run，所以事件堆积
	for i := 0; i < 3; i++ {
		emitter.Emit(makeEvent(int64(i)))
	}

	snap := metrics.Snapshot()
	if snap.QueueDepth != 3 {
		t.Errorf("QueueDepth 应为 3，实际 %d", snap.QueueDepth)
	}
}

var metricCounter atomic.Int64

// uniqueMetricName 用单调递增数字生成全局唯一后缀。
// expvar 的 var 名字必须全局唯一——同一名字被 Publish 两次会 panic。
func uniqueMetricName(prefix string) string {
	return prefix + "_" + strconv.FormatInt(metricCounter.Add(1), 10)
}

// TestMetrics_AddRedactErrors_NilReceiver 验证 nil receiver 不 panic。
func TestMetrics_AddRedactErrors_NilReceiver(_ *testing.T) {
	var m *Metrics
	m.AddRedactErrors(1)
}

// TestMetrics_AddEmitErrors_NilReceiver 验证 nil receiver 不 panic。
func TestMetrics_AddEmitErrors_NilReceiver(_ *testing.T) {
	var m *Metrics
	m.AddEmitErrors(1)
}

// TestMetrics_AddDropped_NilReceiver 验证 nil receiver 不 panic。
func TestMetrics_AddDropped_NilReceiver(_ *testing.T) {
	var m *Metrics
	m.AddDropped(1)
}

// TestMetrics_SetQueueDepth_NilReceiver 验证 nil receiver 不 panic。
func TestMetrics_SetQueueDepth_NilReceiver(_ *testing.T) {
	var m *Metrics
	m.SetQueueDepth(10)
}

// 编译期检查 runevent 仍在引用。
var _ = runevent.KindModelTurn
var _ = context.Background
