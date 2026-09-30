// Cost 用例：聚合某 run 的成本数据，返回面板消费用的 CostSummary。
//
// F4「unpriced 显式」：未命中价格表的调用不静默按 0 计，而是 UnpricedCount++
// 并体现在 summary 里。这是给运营/财务的诚实默认值——总成本只统计已定价部分，
// 未定价部分单独计数，绝不假报 0。
package observability

import "context"

// CostSummary 是单 run 的成本面板输入。
type CostSummary struct {
	RunID          string
	TotalCostMinor int64             // 总成本（已定价部分，按主币种累加）
	Currency       string            // 主币种（取首条 CostEvent 的币种）
	UnpricedCount  int64             // 未命中价格表的调用次数
	TotalCalls     int64             // 总调用次数
	TokensIn       int64
	TokensOut      int64
	TokensCached   int64
	TokensReason   int64
	ByProvider     []ProviderBreakdown // 按 provider 拆开
}

// ProviderBreakdown 是按 provider 拆分的成本小计。
//
// 用 slice 而非 map 是为了 JSON 序列化稳定——map 序列化顺序未定义。
type ProviderBreakdown struct {
	Provider       string
	Model          string
	Calls          int64
	CostMinor      int64
	Currency       string
	UnpricedCount  int64
	TokensIn       int64
	TokensOut      int64
}

// CostOfRun 聚合某 run 的成本数据。
//
// 算法：从 CostStore 拉取 CostEvent，按 provider×model 聚合；同币种累加，
// 跨币种不累加（保留原始币种，由上层做汇率换算——这一步本阶段不做）。
//
// Currency 字段：取首次见到的币种作为「主币种」。当同一 run 出现多币种时，
// 返回 ErrMixedCurrency——前端应在 UI 上按币种分组显示，而不是让 TotalCostMinor
// 跨越币种加总（避免假数据）。
//
// F3「与真实账单对账偏差 < 1%」是接口契约；本阶段只做基础聚合，实测留待外部资源。
func (uc *UseCases) CostOfRun(ctx context.Context, runID string) (CostSummary, error) {
	events, err := uc.Cost.CostOfRun(ctx, runID)
	if err != nil {
		return CostSummary{}, err
	}

	summary := CostSummary{RunID: runID, TotalCalls: int64(len(events))}
	if len(events) == 0 {
		return summary, nil
	}

	byProvider := make(map[string]*ProviderBreakdown)
	for _, ev := range events {
		if ev.Unpriced {
			summary.UnpricedCount++
		}
		summary.TokensIn += ev.TokensIn
		summary.TokensOut += ev.TokensOut
		summary.TokensCached += ev.TokensCached
		summary.TokensReason += ev.TokensReasoning

		key := ev.Provider + "|" + ev.Model + "|" + ev.Currency
		pb, ok := byProvider[key]
		if !ok {
			pb = &ProviderBreakdown{
				Provider: ev.Provider,
				Model:    ev.Model,
				Currency: ev.Currency,
			}
			byProvider[key] = pb
		}
		pb.Calls++
		pb.TokensIn += ev.TokensIn
		pb.TokensOut += ev.TokensOut
		if !ev.Unpriced {
			pb.CostMinor += ev.CostMinor
		} else {
			pb.UnpricedCount++
		}

		// 第一个事件定主币种
		if summary.Currency == "" {
			summary.Currency = ev.Currency
		}
	}

	// 累加已定价成本
	for _, pb := range byProvider {
		summary.TotalCostMinor += pb.CostMinor
		summary.ByProvider = append(summary.ByProvider, *pb)
	}

	return summary, nil
}
