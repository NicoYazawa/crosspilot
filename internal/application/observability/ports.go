// Package observability 是 P5 阶段的可观测三件套用例层。
//
// 名称同 agent/observability 与 infra/observability，但角色不同：
//   - agent/observability 是「事件发射器」（生产端）
//   - infra/observability 是「脱敏器、Sink 实现」（基础设施）
//   - application/observability 是「回放/差分/成本归因/A/B」用例（消费端）
//
// 本包不写 HTTP 处理器——presentation/observability 是它的 HTTP 适配器。
package observability

import (
	"context"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// JournalStore 是事件日志的读端口。
//
// 不直接 import agui.JournalStore 是为了避免 application → presentation 依赖；
// 容器装配阶段把 agui.JournalStore 实例注入到这里。
//
// 这是 P5 D7「回放 API 直接复用 P4 journal」决策的物理保证。
type JournalStore interface {
	Append(ctx context.Context, ev runevent.Event) (int64, error)
	Since(ctx context.Context, runID string, since int64, limit int) ([]runevent.Event, error)
	LastSeq(ctx context.Context, runID string) (int64, error)
}

// CostStore 是成本归因数据的端口。
//
// F4「unpriced 显式」的语义在这里固化：未命中价格表的调用标 unpriced=true
// 而不是按 0 计费。PriceBook 注入本包，由容器装配时挂上。
type CostStore interface {
	// CostOfRun 返回该 run 的全部成本事件。
	//
	// 每条 event 对应一次模型调用，event 里携带 provider/model/token 计数
	// 与 unpriced 标记。
	CostOfRun(ctx context.Context, runID string) ([]CostEvent, error)
}

// CostEvent 是单次模型调用的成本记录。
//
// 字段都从 observability.cost 表出，币种以最小单位计（F3 留待外部资源验证）。
//
// 注：P6 起统一 snake_case JSON tag。
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
	Unpriced        bool   `json:"unpriced"` // F4 闸门：true 表示未命中价格表
}

// ExperimentStore 是 A/B 实验元数据端口。
//
// F8「LLM-judge κ ≥ 0.7 才启用」是接口契约——实现留到 judge 模型就绪之后。
type ExperimentStore interface {
	// ArmFor 返回 run_id 所属的实验臂。
	//
	// 当 run 未注册到任何实验时返回 ("", false)。
	ArmFor(ctx context.Context, runID string) (arm string, ok bool, err error)
	// ArmsSummary 返回实验各臂的聚合统计。
	ArmsSummary(ctx context.Context, key string) ([]ArmSummary, error)
}

// ArmSummary 是实验臂的聚合统计。
//
// Calls / LatencyP95 / CostTotal / UnpricedCount 是 F10「与 token 无关」的核心
// 指标——即便跨协议族 token 口径不同，这四条仍可比。
//
// 注：P6 起统一 snake_case JSON tag。Judge 评分（F8 缺口）不暴露字段。
type ArmSummary struct {
	Arm            string `json:"arm"`
	Calls          int64  `json:"calls"`
	LatencyP95Ms   int64  `json:"latency_p95_ms"`
	CostTotalMinor int64  `json:"cost_total_minor"`
	Currency       string `json:"currency"`
	UnpricedCount  int64  `json:"unpriced_count"`
}

// Clock 提供当前时间；为 nil 时使用 time.Now。
type Clock interface {
	Now() time.Time
}

// wallClock 是 Clock 的默认实现。
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }
