// Package observability 是可观测三件套跨层共享的数据形状。
//
// 为什么这些类型不在声明端口的包里：端口在消费者一侧（agent/observability
// 声明 Sink，application/observability 声明 CostStore），而实现它们在
// 基础设施层（Postgres 适配器）。若记录类型跟着端口留在 agent / application，
// 基础设施层就必须反向依赖它们——那正是依赖方向检查禁止的一条边。
//
// 这与交易链路是同一个模式：application/trade 与 infra/persistence/pg 都
// 依赖 domain/trade 的类型，谁也不必认识谁。因此这里只放「穿过端口的数据
// 形状」，不放任何行为：领域层不因为多了一个成本事件而获得业务规则。
package observability

// SinkRecord 是观测事件写入存储时的形态。
//
// PayloadRedacted 是已脱敏的 JSON（脱敏在写路径完成，见 F7 闸门）。
// PayloadSHA256 是脱敏后内容的 SHA-256，用于完整性校验与去重。
// PayloadSize 是原始 payload 的字节数，与 SHA256 一起构成本文寻址，
// 使原始 blob 不必落库也能验证内容是否被替换。
type SinkRecord struct {
	EventID         string
	RunID           string
	Seq             int64
	Kind            string
	Agent           string
	PayloadSHA256   string
	PayloadSize     int
	PayloadRedacted []byte
	// CreatedAt 是 Unix 秒。用整数而不是 time.Time：这个结构要能被序列化、
	// 比对与跨进程传递，时间的时区与单调时钟位在数据库里没有意义。
	CreatedAt int64
	// Cost 是本条事件归属的成本行；nil 表示这条事件不产生成本（非模型调用，
	// 或未启用定价）。
	//
	// 不变量：非 nil 时，Sink 必须在写入本行 event 的**同一个事务内**写
	// observability.cost——cost.event_id 有外键指向 event.event_id，跨事务写
	// 会在事件尚未提交时撞外键。
	Cost *CostEvent
}

// CostEvent 是单次模型调用的成本记录。
//
// 字段都从 observability.cost 表出，币种以最小单位计（F3 留待外部资源验证）。
//
// Unpriced 是 F4 闸门的载体：未命中价格表的调用必须显式标 true，绝不允许
// 「未定价就当 0 元」——两者的区别是「我们知道这次调用不花钱」与
// 「我们不知道它花了多少钱」，把后者记成 0 会让成本看板长期低报。
type CostEvent struct {
	EventID         string `json:"event_id"`
	RunID           string `json:"run_id"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	TokensIn        int64  `json:"tokens_in"`
	TokensOut       int64  `json:"tokens_out"`
	TokensCached    int64  `json:"tokens_cached"`
	TokensReasoning int64  `json:"tokens_reasoning"`
	CostMinor       int64  `json:"cost_minor"`
	Currency        string `json:"currency"`
	Unpriced        bool   `json:"unpriced"`
}

// ArmSummary 是实验臂的聚合统计。
//
// Calls / LatencyP95Ms / CostTotalMinor / UnpricedCount 是 F10「与 token 无关」
// 的核心指标——即便跨协议族 token 口径不同，这四条仍可比。判分（F8）不在
// 这里：LLM-judge 未就绪，先留缺口而不是先造一个假的分数。
type ArmSummary struct {
	Arm            string `json:"arm"`
	Calls          int64  `json:"calls"`
	LatencyP95Ms   int64  `json:"latency_p95_ms"`
	CostTotalMinor int64  `json:"cost_total_minor"`
	Currency       string `json:"currency"`
	UnpricedCount  int64  `json:"unpriced_count"`
}
