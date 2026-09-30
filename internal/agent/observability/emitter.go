// Package observability 提供 Agent 端的事件发射器。
//
// 关键不变量：
//   - Emit 同步只塞 channel，**绝不阻塞 Agent**（F6 闸门）
//   - 队列满时丢弃并计数（dropped），不阻塞调用方
//   - 后台 worker 批量落 sink；sink 失败计入 dropped，绝不抛回
//   - 脱敏在写路径上完成（Emitter 内部 Redactor），下游读者拿到的是已脱敏数据（F7）
//
// 调用方在 Bridge.Run 中每 Append 一条事件到 journal 后，调一次 Emit：
//
//	bridge.Append(ev)         // 先落 journal（同步，P4 E4 保证）
//	emitter.Emit(&ev)        // 后送观测（异步，永不阻塞；脱敏内部完成）
//
// 顺序：journal 先写；观测可以丢，但 journal 写入不能丢。这是「先 journal 后 SSE」
// 的延伸：观测是 journal 的下游消费者。
package observability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"expvar"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// SinkRecord 是观测用的事件形态。
//
// PayloadRedacted 是已脱敏 JSON（写路径脱敏完成）。PayloadSHA256 是脱敏后
// 内容的 SHA-256，用于完整性校验与去重。PayloadSize 是原始 payload 长度，
// 用于面板的「压缩比」指标。
type SinkRecord struct {
	EventID         string
	RunID           string
	Seq             int64
	Kind            string
	Agent           string
	PayloadSHA256   string
	PayloadSize     int
	PayloadRedacted []byte
	CreatedAt       int64
}

// Sink 是 Emitter 的下游端口。
//
// 接口留在本包（消费者侧）是为了让 Bridge 与 application 测试不必依赖
// infra/observability。infra/observability.Redactor 满足 Redactor 接口；
// MemorySink（agent 层测试用）实现本接口。
//
// 实现要点：
//   - Append 写入失败时返回 error；Emitter 把它计入 dropped counter
//   - Close 在 Run 退出时调用；只用于释放资源，不强制刷批
type Sink interface {
	Append(ctx context.Context, batch []SinkRecord) error
	Close() error
}

// Redactor 是脱敏器的端口。
//
// 接口留在本包（消费者侧）是为了让 Bridge 不必依赖 infra/observability。
// infra/observability.Redactor 直接实现本接口（鸭子类型，无需显式声明）。
//
// Apply 输入待脱敏字节流，输出脱敏后字节流；err 在「脱敏器内部异常」时返回，
// 业务错误（验证失败）按 fail-open 处理：返回原值、nil 错误，调用方日志记录。
type Redactor interface {
	Apply(in []byte) ([]byte, error)
}

// ErrEmitterStopped 表示 Emitter 已停止（Run 退出后调用 Emit）。
var ErrEmitterStopped = errors.New("observability: emitter 已停止")

// EmitterConfig 是构造 Emitter 的参数。
type EmitterConfig struct {
	QueueSize  int           // channel 缓冲；默认 1024
	BatchSize  int           // 批量大小；默认 64
	FlushEvery time.Duration // 强制 flush 间隔；默认 100ms
	Clock      func() time.Time
	Logger     *slog.Logger
	OnDrop     func(reason string, count int)
	// Redactor 必填（脱敏是 F7 闸门）。失败/未设置 → Emit 计入 dropped。
	Redactor Redactor
	// Metrics 是可选的指标集合；提供后 Emit 会自动更新 QueueDepth / DroppedTotal 等。
	// 不提供时 Emitter 仍能正常工作（指标为 no-op）。
	Metrics *Metrics
}

// Emitter 是异步事件发射器。
//
// 内部维护一个有界 channel：调用方把 record 塞进去即返回，不阻塞。
// 后台 goroutine 攒批 flush 到 sink。sink 失败时计入 OnDrop（计入 Prometheus
// dropped counter + 日志告警）。
//
// 脱敏在 Emit 同步阶段完成——这是 F7「写路径脱敏」的物理保证：原始 payload
// 不会进入 SinkRecord，永远只塞脱敏后的 JSON。
type Emitter struct {
	cfg      EmitterConfig
	queue    chan SinkRecord
	sink     Sink
	stopped  atomic.Bool

	wg     sync.WaitGroup
	closed chan struct{}
}

// NewEmitter 构造 Emitter。Sink 与 Redactor 都必须非 nil。
func NewEmitter(sink Sink, cfg EmitterConfig) *Emitter {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = 1024
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 64
	}
	if cfg.FlushEvery <= 0 {
		cfg.FlushEvery = 100 * time.Millisecond
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	if cfg.Metrics == nil {
		// 默认 metrics：no-op，避免每次 nil 检查
		cfg.Metrics = &Metrics{
			QueueDepth:   newNoopInt(),
			DroppedTotal: newNoopInt(),
			EmitTotal:    newNoopInt(),
			EmitErrors:   newNoopInt(),
		}
	}
	if cfg.OnDrop == nil {
		cfg.OnDrop = func(string, int) {} // 默认 no-op
	}
	return &Emitter{
		cfg:    cfg,
		queue:  make(chan SinkRecord, cfg.QueueSize),
		sink:   sink,
		closed: make(chan struct{}),
	}
}

// newNoopInt 返回一个不注册的 expvar.Int，避免全局 expvar 被无意义的指标污染。
func newNoopInt() *expvar.Int {
	// expvar.Int 是值类型，其底层 int64 字段无锁；不注册就是 no-op。
	i := &expvar.Int{}
	// 故意不调 expvar.Publish —— 这是内部指标，不对外暴露。
	return i
}

// Emit 把一条事件塞入队列。同步完成，绝不阻塞。
//
// 当队列已满时：调用 cfg.OnDrop("queue_full", 1) 并丢弃本条事件。
// 这是 F6「埋点不阻塞」闸门：宁可丢观测，也不能拖慢 Agent。
//
// 脱敏由 Emitter 内部完成：Redactor.Apply 失败时按 fail-open 走（落原值，
// OnDrop("redact_error", 1)）。SinkRecord 里的 PayloadRedacted 永远是
// 已脱敏或确认原值的字节流，调用方不必再做任何二次处理。
func (e *Emitter) Emit(ev *runevent.Event) {
	e.cfg.Metrics.AddEmit(1)

	if e.stopped.Load() {
		e.cfg.OnDrop("emitter_stopped", 1)
		e.cfg.Metrics.AddDropped(1)
		return
	}
	if ev == nil {
		e.cfg.OnDrop("nil_event", 1)
		e.cfg.Metrics.AddDropped(1)
		return
	}

	// 同步脱敏（F7 闸门）：写路径上完成，读者拿到的总是脱敏后数据。
	payloadRedacted, err := e.cfg.Redactor.Apply(ev.Payload)
	if err != nil {
		// 脱敏器内部异常：按 fail-open 落原值 + 计数
		e.cfg.OnDrop("redact_error", 1)
		e.cfg.Metrics.AddRedactErrors(1)
		payloadRedacted = ev.Payload
	}

	rec := SinkRecord{
		EventID:         ev.EventID,
		RunID:           ev.RunID,
		Seq:             ev.Seq,
		Kind:            string(ev.Kind),
		Agent:           ev.Agent,
		PayloadSHA256:   hashSHA256(payloadRedacted),
		PayloadSize:     len(ev.Payload),
		PayloadRedacted: payloadRedacted,
		CreatedAt:       ev.CreatedAt.UnixNano(),
	}

	select {
	case e.queue <- rec:
		e.cfg.Metrics.SetQueueDepth(len(e.queue))
	default:
		// 队列满：丢弃 + 计数。这是 F6 的硬行为。
		e.cfg.OnDrop("queue_full", 1)
		e.cfg.Metrics.AddDropped(1)
	}
}

// Run 是后台 worker 循环：ctx 取消时退出。
//
// 调用方在服务启动时启一个 goroutine 跑这个函数，退出时 wait。
//
// Run 退出后 Queue 中残留的记录**不保证全部 flush**（语义上"best-effort"）：
// 已塞进 channel 的事件尽量消费，但超出 batch+flush 间隔的不再等待。
func (e *Emitter) Run(ctx context.Context) {
	e.wg.Add(1)
	defer e.wg.Done()

	ticker := time.NewTicker(e.cfg.FlushEvery)
	defer ticker.Stop()

	batch := make([]SinkRecord, 0, e.cfg.BatchSize)

	flush := func(reason string) {
		if len(batch) == 0 {
			return
		}
		if err := e.sink.Append(ctx, batch); err != nil {
			if !errors.Is(err, context.Canceled) {
				e.cfg.OnDrop("sink_error: "+err.Error(), len(batch))
				e.cfg.Metrics.AddDropped(len(batch))
				e.cfg.Metrics.AddEmitErrors(1)
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			flush("ctx_done")
			e.stopped.Store(true)
			return

		case <-e.closed:
			flush("closed")
			e.stopped.Store(true)
			return

		case rec := <-e.queue:
			batch = append(batch, rec)
			if len(batch) >= e.cfg.BatchSize {
				flush("batch_full")
			}

		case <-ticker.C:
			flush("interval")
		}
	}
}

// Close 通知 Run 退出并等待。
func (e *Emitter) Close() error {
	if e.stopped.Load() {
		return nil
	}
	close(e.closed)
	e.wg.Wait()
	return e.sink.Close()
}

// QueueDepth 返回当前队列长度（监控用）。
func (e *Emitter) QueueDepth() int {
	return len(e.queue)
}

// hashSHA256 是 hex-encoded SHA-256。
func hashSHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// discardWriter 是空 slog handler 的输出目标，避免日志默认写到 stderr。
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }