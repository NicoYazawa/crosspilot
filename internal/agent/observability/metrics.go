// Metrics 是观测通道的运行时计数。
//
// P5 计划 §3 F10「跨协议族可比」要求面板除了 token 类口径，还要给出
// 与 token 无关的指标——本文件的 QueueDepth、DroppedTotal、EmitTotal、
// EmitErrorsTotal 就是这类指标：跨任何厂商的 LLM 调用都用同一份数字。
//
// 实现：直接用 stdlib expvar（不依赖 prometheus 客户端）。expvar 在 /debug/vars
// 暴露；HTTP 适配由 presentation 层负责（见 presentation/observability.Routes）。
//
// 这是「不依赖外部观测栈也能跑」的物理保证——P5 阶段先把指标形态定下来，
// 后期接 prometheus 时只需新增一个 handler，不必改 Emitter 计数路径。
package observability

import (
	"expvar"
	"sync/atomic"
)

// Metrics 是观测通道的计数器集合。
//
// Emitter 持有一个；多个 Emitter 实例时各自一份（按 sink 分桶）。
// 调用方通过 Snapshot() 读快照，不会阻塞 Emitter 自身。
type Metrics struct {
	QueueDepth   *expvar.Int // 当前 channel 队列长度
	DroppedTotal *expvar.Int // 累计 dropped 事件数（任意 reason）
	EmitTotal    *expvar.Int // 累计 Emit 调用次数（含成功 + dropped）
	EmitErrors   *expvar.Int // 累计 sink 写入错误次数
	redactErrors atomic.Int64 // 累计脱敏失败次数（内部用，不暴露 expvar）
}

// NewMetrics 构造并注册一组 expvar 指标。
//
// 命名规则：observability.{field}，避免与 OTel 标准指标冲突。
func NewMetrics(name string) *Metrics {
	m := &Metrics{
		QueueDepth:   expvar.NewInt(name + "_queue_depth"),
		DroppedTotal: expvar.NewInt(name + "_dropped_total"),
		EmitTotal:    expvar.NewInt(name + "_emit_total"),
		EmitErrors:   expvar.NewInt(name + "_emit_errors_total"),
	}
	return m
}

// AddDropped 累计 dropped 数（供 Emitter 调用）。
func (m *Metrics) AddDropped(n int) {
	if m == nil || m.DroppedTotal == nil {
		return
	}
	m.DroppedTotal.Add(int64(n))
}

// AddEmit 累计 Emit 调用次数。
func (m *Metrics) AddEmit(n int) {
	if m == nil || m.EmitTotal == nil {
		return
	}
	m.EmitTotal.Add(int64(n))
}

// AddEmitErrors 累计 sink 写入错误次数。
func (m *Metrics) AddEmitErrors(n int) {
	if m == nil || m.EmitErrors == nil {
		return
	}
	m.EmitErrors.Add(int64(n))
}

// AddRedactErrors 累计脱敏失败次数。
func (m *Metrics) AddRedactErrors(n int) {
	if m == nil {
		return
	}
	m.redactErrors.Add(int64(n))
}

// SetQueueDepth 设置当前队列深度。
func (m *Metrics) SetQueueDepth(depth int) {
	if m == nil || m.QueueDepth == nil {
		return
	}
	m.QueueDepth.Set(int64(depth))
}

// Snapshot 返回当前指标的快照（JSON 友好）。
type MetricsSnapshot struct {
	QueueDepth   int64 `json:"queue_depth"`
	DroppedTotal int64 `json:"dropped_total"`
	EmitTotal    int64 `json:"emit_total"`
	EmitErrors   int64 `json:"emit_errors_total"`
	RedactErrors int64 `json:"redact_errors"`
}

// Snapshot 读取当前计数（不阻塞）。
func (m *Metrics) Snapshot() MetricsSnapshot {
	if m == nil {
		return MetricsSnapshot{}
	}
	return MetricsSnapshot{
		QueueDepth:   m.QueueDepth.Value(),
		DroppedTotal: m.DroppedTotal.Value(),
		EmitTotal:    m.EmitTotal.Value(),
		EmitErrors:   m.EmitErrors.Value(),
		RedactErrors: m.redactErrors.Load(),
	}
}
