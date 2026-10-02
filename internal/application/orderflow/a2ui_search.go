// 商品检索结果的 A2UI 报文生成器。
//
// 与 a2ui.go（确认单）同构：三报文按 createSurface → updateComponents →
// updateDataModel 顺序产出，每条都过 runevent 的校验，校验不过就不发。
//
// 与确认单那套的唯一差别是数据来源——这里的候选来自 product_search_tool 的
// 返回，而不是一笔待确认的交易。

package orderflow

import (
	"encoding/json"
	"fmt"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// ProductHitLike 是 A2UI 商品卡关心的最小字段集合。
//
// 与 ConfirmationLike 同样的取舍：不 import catalogsearch.ProductCard。一是
// 分层规则不允许 application 的兄弟子包互相 import；二是 A2UI 只关心「渲染
// 一张卡要什么」，跟着检索用例的领域模型走会让前端契约被后端重构牵着动。
//
// json tag 必须与 ProductCard 的对外契约逐字一致——它反序列化的正是
// product_search_tool 返回的那份 JSON。
type ProductHitLike struct {
	ProductID    string  `json:"product_id"`
	Title        string  `json:"title"`
	Brand        string  `json:"brand"`
	Category     string  `json:"category"`
	PriceMajor   float64 `json:"price_major"`
	Currency     string  `json:"currency"`
	DefaultSKUID string  `json:"default_sku_id"`
	ImageURL     string  `json:"image_url"`
}

// SearchEmitter 把检索命中翻译成 v0.9 报文。
type SearchEmitter struct{}

// NewSearchEmitter 构造一个 emitter。
func NewSearchEmitter() *SearchEmitter { return &SearchEmitter{} }

// Emit 返回 createSurface / updateComponents / updateDataModel 三条消息。
//
// hits 为空时返回 nil（不渲染）：一张商品都没有的一轮检索画不出列表，
// 发一个空 surface 只会把上一轮的候选从页面上抹掉。
func (e *SearchEmitter) Emit(hits []ProductHitLike) ([]map[string]any, error) {
	if len(hits) == 0 {
		return nil, nil
	}

	// 卡片直接挂在根 Column 下，靠 children 的 id 列表挂载。
	//
	// 这里不能放一个 List 组件来装卡片：前端的 List 只渲染 props.items /
	// props.data（纯数据条目），不解析子组件；而且 Renderer 只对 type=Column
	// 的节点下降。用 List 当容器的话卡片组件会全部存在、校验也全过，
	// 但一张都不显示。
	create := map[string]any{
		"action":    runevent.A2UIActionCreateSurface,
		"catalogId": runevent.A2UICatalogID,
		"version":   runevent.A2UIVersion,
		"components": []any{
			a2uiRoot("搜索结果", cardIDs("hit", len(hits)), nil),
		},
	}
	if err := runevent.ValidateCreateSurface(create); err != nil {
		return nil, fmt.Errorf("createSurface 校验失败：%w", err)
	}

	updateComp := map[string]any{
		"action":     runevent.A2UIActionUpdateComponents,
		"catalogId":  runevent.A2UICatalogID,
		"components": buildHitComponents(hits),
	}
	if err := runevent.ValidateUpdateComponents(updateComp); err != nil {
		return nil, fmt.Errorf("updateComponents 校验失败：%w", err)
	}

	updateData := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
		"value": map[string]any{
			"path": runevent.ShoppingRequirementsPath,
			"data": []any{
				map[string]any{
					"key":   "candidates",
					"value": buildHitData(hits),
				},
			},
		},
	}
	if err := runevent.ValidateUpdateDataModel(updateData); err != nil {
		return nil, fmt.Errorf("updateDataModel 校验失败：%w", err)
	}

	return []map[string]any{create, updateComp, updateData}, nil
}

// buildHitComponents 把命中列表翻译成 ProductCard 组件。
//
// id 用 `hit-<index>`：与确认单卡片的 `card-<index>` 分开，避免两次不同来源的
// 渲染互相覆盖组件实例。index 保证幂等——同一份结果重发得到同一批 id。
func buildHitComponents(hits []ProductHitLike) []any {
	out := make([]any, 0, len(hits))
	for i, h := range hits {
		out = append(out, map[string]any{
			"id":      fmt.Sprintf("hit-%d", i),
			"type":    "ProductCard",
			"surface": "dynamic",
			"props": map[string]any{
				"product_id":  h.ProductID,
				"sku_id":      h.DefaultSKUID,
				"title":       h.Title,
				"brand":       h.Brand,
				"category":    h.Category,
				"price_major": h.PriceMajor,
				"currency":    h.Currency,
				"image_url":   h.ImageURL,
			},
		})
	}
	return out
}

// buildHitData 把命中列表翻译成数据条目。
func buildHitData(hits []ProductHitLike) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		out = append(out, map[string]any{
			"product_id":  h.ProductID,
			"sku_id":      h.DefaultSKUID,
			"title":       h.Title,
			"price_major": h.PriceMajor,
			"currency":    h.Currency,
		})
	}
	return out
}

// searchHitsPayload 是 product_search_tool 返回体的最小投影。
type searchHitsPayload struct {
	Hits []ProductHitLike `json:"hits"`
}

// HitsFromToolResult 从工具结果的 JSON 里取回命中列表。
//
// 入参是 orchestrator 的 tool_result 事件 content——它在成功态就是 handler
// 返回的原始 JSON（见 orchestrator.summary）。因此这里不需要 handler 额外
// 传什么，A2UI 是纯粹的展示层关注点，工具本身保持只讲业务。
func HitsFromToolResult(content []byte) ([]ProductHitLike, error) {
	var p searchHitsPayload
	if err := json.Unmarshal(content, &p); err != nil {
		return nil, err
	}
	return p.Hits, nil
}
