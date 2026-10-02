package container

import (
	"context"
	"encoding/json"
	"log/slog"

	agentobs "github.com/NicoYazawa/crosspilot/internal/agent/observability"
	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
	domainobs "github.com/NicoYazawa/crosspilot/internal/domain/observability"
	"github.com/NicoYazawa/crosspilot/internal/pricing"
)

// modelIdentity 是当前生效的 (provider, model)。
//
// 从 buildDecisionProvider 带出来而不是各处重读配置：只有那个函数知道
// 「配置里选了哪家、最后是不是真的接上了」（密钥缺失 / anthropic 未接入时会
// 退回 unavailableModel），在别处再推一遍必然与它对不上。
type modelIdentity struct {
	Provider string
	Model    string
}

// pricingSink 是给观测 Sink 加「成本归因」的装饰器。
//
// 为什么住在 container 而不是 Emitter 或 agent 层：分层规则规定
// internal/pricing 只能被装配根引用（见 tools/arch/arch_test.go），
// 而 Emitter 的职责是「脱敏 + 攒批 + 不阻塞」，不该知道价格表存在。
// 装饰器让两边都保持原样，装配处只多一行。
//
// 为什么在这里算而不是在 Emit 热路径：Append 跑在 Emitter 的后台 flush 协程里，
// 一次批处理几十条；放在 Emit 里则会把查表与 decimal 运算加进 Agent 的调用栈，
// 触碰 F6「埋点不阻塞」闸门。
type pricingSink struct {
	inner agentobs.Sink
	book  *pricing.PriceBook
	// configured 是配置声明的身份，作为 payload 里没带 provider/model 时的兜底。
	configured modelIdentity
	logger     *slog.Logger
}

// newPricingSink 包装 inner。book 为 nil 时返回 inner 原样——
// 定价整个关掉，而不是每批都空跑一遍查表。
func newPricingSink(
	inner agentobs.Sink,
	book *pricing.PriceBook,
	id modelIdentity,
	logger *slog.Logger,
) agentobs.Sink {
	if book == nil || book.Size() == 0 {
		if logger != nil {
			logger.Warn("价格表为空：模型调用的成本将全部记为 unpriced（绝不是 0 元）")
		}
		return inner
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &pricingSink{inner: inner, book: book, configured: id, logger: logger}
}

// Append 给批内每条模型调用补上成本行，再交给下游写库。
//
// 只处理 model_turn：其余事件（工具调用、A2UI 报文、run 起止）不产生 token 费用，
// 给它们各写一行全 0 的成本会让「不涉及计费」与「花了 0 元」在表里长得一样，
// 成本看板的 total_calls 也会虚高。
func (s *pricingSink) Append(ctx context.Context, batch []domainobs.SinkRecord) error {
	for i := range batch {
		rec := &batch[i]
		if rec.Kind != orchestrator.KindModelTurn {
			continue
		}
		rec.Cost = s.quote(ctx, rec)
	}
	return s.inner.Append(ctx, batch)
}

// Close 透传给下游。装饰器不持有资源。
func (s *pricingSink) Close() error { return s.inner.Close() }

// quote 从一条已脱敏的事件 payload 里取出用量并定价。
//
// 读的是 PayloadRedacted 而不是原始 payload：那正是写进库里的那份字节，
// 用它定价能保证「表里存着的用量」与「表里存着的费用」来自同一份输入。
// （脱敏器只替换敏感字段的**值**，不动键名与数值型字段。）
func (s *pricingSink) quote(ctx context.Context, rec *domainobs.SinkRecord) *domainobs.CostEvent {
	var payload orderflow.ModelTurnPayload
	if err := json.Unmarshal(rec.PayloadRedacted, &payload); err != nil {
		// payload 解不开：仍要落一条 unpriced 成本。返回 nil 会让这条调用在
		// 成本表里彻底消失，那比「有一条标着不知道多少钱的记录」更糟。
		s.logger.WarnContext(ctx, "观测事件 payload 解析失败，按未定价记录",
			slog.String("event_id", rec.EventID), slog.String("error", err.Error()))
		return s.unpriced(rec, s.configured)
	}

	provider, model := payload.Provider, payload.Model
	if provider == "" {
		provider = s.configured.Provider
	}
	if model == "" {
		model = s.configured.Model
	}

	cost := &domainobs.CostEvent{
		EventID:  rec.EventID,
		RunID:    rec.RunID,
		Provider: provider,
		Model:    model,
	}
	// usage 缺失 = 上游没返回用量 = 「我们不知道花了多少」，不是「不花钱」。
	// 这条同样要落库并标 unpriced（F4 闸门）。
	if payload.Usage == nil {
		cost.Unpriced = true
		return cost
	}
	cost.TokensIn = payload.Usage.InputTokens
	cost.TokensOut = payload.Usage.OutputTokens
	cost.TokensCached = payload.Usage.CachedTokens
	cost.TokensReasoning = payload.Usage.ReasoningTokens

	q, err := s.book.Price(provider, model, cost.TokensIn, cost.TokensOut, cost.TokensCached)
	if err != nil {
		// 算不出来（decimal 溢出一类）与「没价目」是两件事：前者是 bug，
		// 要留下日志；两者都按 unpriced 落库，因为都不该被当成 0 元。
		s.logger.Error("成本计算失败，按未定价记录",
			slog.String("event_id", rec.EventID),
			slog.String("provider", provider),
			slog.String("model", model),
			slog.Any("error", err))
		cost.Unpriced = true
		return cost
	}
	cost.CostMinor = q.CostMinor
	cost.Currency = q.Currency
	cost.Unpriced = q.Unpriced
	return cost
}

// unpriced 构造一条「知道用量未知、且不打算猜」的成本行。
func (s *pricingSink) unpriced(rec *domainobs.SinkRecord, id modelIdentity) *domainobs.CostEvent {
	return &domainobs.CostEvent{
		EventID:  rec.EventID,
		RunID:    rec.RunID,
		Provider: id.Provider,
		Model:    id.Model,
		Unpriced: true,
	}
}
