package container

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/NicoYazawa/crosspilot/internal/agent/tools"
	"github.com/NicoYazawa/crosspilot/internal/application/catalogsearch"
	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// product_search_tool 的缺省值。
//
// schema 里虽然写了 default，但「默认」只是写给模型的建议：模型省略该字段时
// JSON 里根本没有它，反序列化得到零值，而 ProductSearchSpec.Validate 要求
// TopK ∈ [1,100]，0 会当场报错。默认值必须在 handler 里补，不能指望模型一定带上。
const (
	defaultTopK          = 5
	defaultCurrencyCNY   = "CNY"
	productSearchToolFmt = "product_search_tool: "
)

// 为什么 handler 放在装配根而不是 internal/application/agenttools：
// 分层规则里 application 组件的 projectDeps 只有 domain 与 agent，**不含
// internal/application 自身**，因此 application 层的包不能 import catalogsearch
// （见 tools/arch/arch_test.go）。而 handler 恰好既要用 catalogsearch.UseCase，
// 又要产出 agent 层的 tools.ToolResult。container 是唯一被允许同时看见两者的
// 地方，它的职责本来就是「把接不上的东西接起来」。
//
// 这些函数是薄适配层：解码参数 → 调用用例 → 按前端契约组结果，不含业务规则。

// productSearcher 是 product_search_tool 需要的最小能力面。
//
// 只声明一个方法：接口窄一分，测试里的替身就少写一分，将来换实现也不用
// 迁就多余的方法。
type productSearcher interface {
	Execute(ctx context.Context, spec catalog.ProductSearchSpec) (catalogsearch.SearchResult, error)
}

// productSearchArgs 与 product_search_tool 的 JSON Schema 逐字对齐。
//
// 参数名是对外契约，改一个字就要同步改 schema 与提示词，否则模型传的
// 永远是旧名字。
type productSearchArgs struct {
	NormalizedQuery      string   `json:"normalized_query"`
	Category             string   `json:"category"`
	ShipTo               string   `json:"ship_to"`
	TopK                 int      `json:"top_k"`
	PriceMaxMajor        *float64 `json:"price_max_major"`
	TargetCurrency       string   `json:"target_currency"`
	ExcludedMaterialTags []string `json:"excluded_material_tags"`
	RequiredMaterialTags []string `json:"required_material_tags"`
	ProductID            string   `json:"product_id"`
	SkuID                string   `json:"sku_id"`
}

// newProductSearchHandler 把模型给的参数翻译成检索用例的输入。
func newProductSearchHandler(uc productSearcher) tools.ToolHandler {
	return func(ctx context.Context, raw json.RawMessage) (tools.ToolResult, error) {
		var a productSearchArgs
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &a); err != nil {
				//nolint:nilerr // 参数不合规是模型能自我纠正的中间状态，以工具结果回给它，而不是打死整轮 run
				return toolFailure(productSearchToolFmt + "参数不是合法 JSON: " + err.Error()), nil
			}
		}

		// 先去掉首尾空白再判空：只有空格的检索词与空串在语义上等价，但直接
		// 比较会放它过关，然后 tokenize 出一批空白 token 去全表扫一遍。
		a.NormalizedQuery = strings.TrimSpace(a.NormalizedQuery)

		// 既没有检索词也没有商品标识，检索出来必然是空集。提前拒绝能让模型
		// 立刻看到原因并补参数，比返回一堆空 hits 更有指导性。
		if a.NormalizedQuery == "" && a.ProductID == "" && a.SkuID == "" {
			return toolFailure(productSearchToolFmt +
				"至少需要 normalized_query、product_id、sku_id 之一"), nil
		}

		topK := a.TopK
		if topK == 0 {
			topK = defaultTopK
		}
		currency := a.TargetCurrency
		if currency == "" {
			currency = defaultCurrencyCNY
		}

		result, err := uc.Execute(ctx, catalog.ProductSearchSpec{
			NormalizedQuery:      a.NormalizedQuery,
			Category:             a.Category,
			ShipTo:               a.ShipTo,
			TopK:                 topK,
			PriceMaxMajor:        a.PriceMaxMajor,
			TargetCurrency:       catalog.Currency(currency),
			ExcludedMaterialTags: a.ExcludedMaterialTags,
			RequiredMaterialTags: a.RequiredMaterialTags,
			ProductID:            a.ProductID,
			SkuID:                a.SkuID,
		})
		if err != nil {
			// 检索失败是业务结果（参数不合规、账本查询出错），不是进程故障：
			// 交给模型转述给买家，而不是把整轮 ReAct 打死。
			//nolint:nilerr // 同上：这个 err 是业务结论，不是本层的失败
			return toolFailure(productSearchToolFmt + err.Error()), nil
		}

		return toolSuccess(searchResultPayload(result))
	}
}

// searchResultPayload 按前端的 snake_case 契约组结果。
//
// 为什么手工组 map 而不是直接 marshal SearchResult：SearchResult 自身没有
// json tag，直接序列化会得到 "Hits" / "RecallStrategy" 这样的大写键名，
// 过不了 tools.ToolRequiredFields 的小写校验。有 tag 的子类型
// （ProductCard / FilteredOut）原样放进去即可。
func searchResultPayload(r catalogsearch.SearchResult) map[string]any {
	hits := r.Hits
	if hits == nil {
		// nil 会被 marshal 成 null。契约里 hits 是数组，给 [] 比给 null 更好：
		// 模型与前端都不必再判一次空。
		hits = []catalogsearch.ProductCard{}
	}
	return map[string]any{
		"hits":                hits,
		"recall_strategy":     r.RecallStrategy,
		"total_candidates":    r.TotalCandidates,
		"rerank_applied":      r.RerankApplied,
		"filtered_out":        r.FilteredOut,
		"missing_identifiers": r.MissingIdentifiers,
		"existence_checked":   r.ExistenceChecked,
	}
}

// toolSuccess 构造一个成功结果。
func toolSuccess(payload map[string]any) (tools.ToolResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		// 上面全是 string/int/bool/结构体，marshal 不会失败；真失败了也不能
		// 返回半截 JSON 给模型的工具消息。
		//nolint:nilerr // 同上：宁可当业务结果回传，也不返回半截 JSON
		return toolFailure("结果序列化失败: " + err.Error()), nil
	}
	return tools.ToolResult{State: tools.ResultStateSuccess, Content: body}, nil
}

// toolFailure 构造一个「业务失败」的工具结果。
//
// 关键：Go error 返回 nil。orchestrator 在 execErr != nil 时会中止整个 run，
// 而「参数不对」「没搜到」都属于模型可以自我纠正的中间状态——应当作为工具
// 结果喂回给它，而不是把这一轮意图直接打死。
func toolFailure(msg string) tools.ToolResult {
	body, _ := json.Marshal(map[string]string{"error": msg})
	return tools.ToolResult{State: tools.ResultStateError, Error: msg, Content: body}
}
