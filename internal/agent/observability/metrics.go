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
	"sync"
	"sync/atomic"
)

// Metrics 是观测通道的计数器集合。
//
// 调用方通过 Snapshot() 读快照，不会阻塞 Emitter 自身。
//
// 命名是进程全局的：expvar 注册表本身只有一份扁平命名空间（/debug/vars 就是
// 它），所以同一个 name 构造出的多个 Metrics 共享同一组计数器，而不是各自
// 分桶。这是 expvar 的模型，不是本类型的取舍——要按 sink 分桶就得换一套
// 带标签的指标库（P5 明确不引入 prometheus 客户端）。
type Metrics struct {
	QueueDepth   *expvar.Int  // 当前 channel 队列长度
	DroppedTotal *expvar.Int  // 累计 dropped 事件数（任意 reason）
	EmitTotal    *expvar.Int  // 累计 Emit 调用次数（含成功 + dropped）
	EmitErrors   *expvar.Int  // 累计 sink 写入错误次数
	redactErrors atomic.Int64 // 累计脱敏失败次数（内部用，不暴露 expvar）
}

// metricsMu 串行化注册，让 NewMetrics 在并发调用下也不会撞进 expvar 的
// 「名字已注册」panic。
var metricsMu sync.Mutex

// NewMetrics 构造并注册一组 expvar 指标。
//
// 命名规则：{name}_{field}。
//
// 幂等：同一个 name 重复调用返回同一组计数器。expvar.NewInt 对已存在的名字
// 直接 panic，而进程里可能不止装配一次（测试、以及将来多实例同进程部署），
// 让一个「读指标」的动作把进程打挂是不可接受的失败模式。
func NewMetrics(name string) *Metrics {
	metricsMu.Lock()
	defer metricsMu.Unlock()
	return &Metrics{
		QueueDepth:   newExpvarInt(name + "_queue_depth"),
		DroppedTotal: newExpvarInt(name + "_dropped_total"),
		EmitTotal:    newExpvarInt(name + "_emit_total"),
		EmitErrors:   newExpvarInt(name + "_emit_errors_total"),
	}
}

// newExpvarInt 取回已注册的计数器，没有才新建。
//
// 调用方须持有 metricsMu。类型不符时（同名变量被别人注册成非 Int）退化成
// 新建前的原样报错：这种情况下「指标错乱」比「启动时明确 panic」更难查。
func newExpvarInt(name string) *expvar.Int {
	if v := expvar.Get(name); v != nil {
		if i, ok := v.(*expvar.Int); ok {
			return i
		}
	}
	return expvar.NewInt(name)
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

// MetricsSnapshot 是当前指标的快照，可直接 JSON 序列化。
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
