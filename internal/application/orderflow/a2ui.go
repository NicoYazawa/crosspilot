// A2UI 报文生成器。
//
// 把 trade.Confirmation 翻译成 A2UI v0.9 三报文：
//
//	createSurface       → 根容器 + 入口组件
//	updateComponents    → 商品卡片列表
//	updateDataModel     → /requirements 数据
//
// 三报文严格按顺序写出，且每条都走 runevent.ValidateXxx 校验——
// 校验失败的报文绝不发出，宁可不渲染也不传坏数据给前端。
package orderflow

import (
	"encoding/json"
	"fmt"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// ConfirmationLike 是 A2UIEmitter 关心的 confirmation 形状。
//
// 故意用接口而不是 trade.Confirmation：本阶段只想拿到最少必要字段；
// P7 真正接 trade 时再换成完整类型。
type ConfirmationLike struct {
	ConfirmationID string
	OperationID    string
	BuyerID        string
	SessionID      string
	Status         string
	ExpiresAt      string
	SubtotalMinor  int64
	Currency       string
	SnapshotHash   string
	Items          []ConfirmationItem
}

type ConfirmationItem struct {
	ProductID  string
	SKUID      string
	Title      string
	Spec       string
	PriceMajor float64
}

// A2UIEmitter 把 confirmation 翻译成 v0.9 报文。
type A2UIEmitter struct{}

// NewA2UIEmitter 构造一个 emitter。
func NewA2UIEmitter() *A2UIEmitter { return &A2UIEmitter{} }

// Emit 返回 createSurface / updateComponents / updateDataModel 三条消息。
//
// 任一报文校验失败时返回错误：调用方应回退到「不渲染」并记录错误，
// 而不是把坏数据塞给前端拒渲染闸门之外的渠道。
func (e *A2UIEmitter) Emit(c ConfirmationLike) ([]map[string]any, error) {
	create := map[string]any{
		"action":    runevent.A2UIActionCreateSurface,
		"catalogId": runevent.A2UICatalogID,
		"version":   runevent.A2UIVersion,
		"components": []any{
			map[string]any{
				"id":   "root",
				"type": "Column",
				"path": runevent.ShoppingRequirementsPath,
				"props": map[string]any{
					"title":           "购物清单",
					"confirmation_id": c.ConfirmationID,
					"expires_at":      c.ExpiresAt,
				},
			},
			map[string]any{
				"id":   "items-list",
				"type": "List",
				"props": map[string]any{
					"placeholder": "已选商品会出现在这里",
				},
			},
			map[string]any{
				"id":   "subtotal-line",
				"type": "SubtotalLine",
				"props": map[string]any{
					"currency": c.Currency,
				},
			},
		},
	}
	if err := runevent.ValidateCreateSurface(create); err != nil {
		return nil, fmt.Errorf("createSurface 校验失败：%w", err)
	}

	updateComp := map[string]any{
		"action":     runevent.A2UIActionUpdateComponents,
		"catalogId":  runevent.A2UICatalogID,
		"components": buildItemComponents(c.Items),
	}
	if len(c.Items) > 0 {
		if err := runevent.ValidateUpdateComponents(updateComp); err != nil {
			return nil, fmt.Errorf("updateComponents 校验失败：%w", err)
		}
	}

	updateData := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
		"value": map[string]any{
			"path": runevent.ShoppingRequirementsPath,
			"data": []any{
				map[string]any{
					"key": "confirmation",
					"value": map[string]any{
						"id":         c.ConfirmationID,
						"operation":  c.OperationID,
						"buyer_id":   c.BuyerID,
						"session_id": c.SessionID,
						"status":     c.Status,
						"expires_at": c.ExpiresAt,
						"subtotal": map[string]any{
							"minor":    c.SubtotalMinor,
							"currency": c.Currency,
						},
						"snapshot_hash": c.SnapshotHash,
					},
				},
				map[string]any{
					"key":   "items",
					"value": buildItemData(c.Items),
				},
			},
		},
	}
	if err := runevent.ValidateUpdateDataModel(updateData); err != nil {
		return nil, fmt.Errorf("updateDataModel 校验失败：%w", err)
	}

	return []map[string]any{create, updateComp, updateData}, nil
}

// buildItemComponents 把商品列表翻译成 ProductCard 组件。
//
// 每张卡片用 `card-<index>` 作 id；幂等性由 index 保证，重发同一 confirmation
// 会得到同样的 id，前端按 id 复用组件实例。
func buildItemComponents(items []ConfirmationItem) []any {
	out := make([]any, 0, len(items))
	for i, item := range items {
		out = append(out, map[string]any{
			"id":      fmt.Sprintf("card-%d", i),
			"type":    "ProductCard",
			"surface": "dynamic",
			"props": map[string]any{
				"product_id":  item.ProductID,
				"sku_id":      item.SKUID,
				"title":       item.Title,
				"spec":        item.Spec,
				"price_major": item.PriceMajor,
			},
		})
	}
	return out
}

// buildItemData 把商品列表翻译成数据条目。
func buildItemData(items []ConfirmationItem) []map[string]any {
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, map[string]any{
			"product_id":  item.ProductID,
			"sku_id":      item.SKUID,
			"title":       item.Title,
			"price_major": item.PriceMajor,
		})
	}
	return out
}

// MarshalPayload 把任意 map 序列化为 RunEvent.Payload 形态。
func MarshalPayload(v map[string]any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}
