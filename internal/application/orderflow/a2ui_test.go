package orderflow_test

import (
	"encoding/json"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
)

// TestA2UIEmit_ThreeMessagesValid 验证 Emit 返回三报文且全部通过 v0.9 校验。
func TestA2UIEmit_ThreeMessagesValid(t *testing.T) {
	c := orderflow.ConfirmationLike{
		ConfirmationID: "conf-001",
		OperationID:    "op-001",
		BuyerID:        "buyer-1",
		SessionID:      "session-1",
		Status:         "pending",
		ExpiresAt:      "2026-09-30T12:10:00Z",
		SubtotalMinor:  29900,
		Currency:       "CNY",
		SnapshotHash:   "abc123",
		Items: []orderflow.ConfirmationItem{
			{ProductID: "P1003", SKUID: "P1003-S1", Title: "防水登山包 30L", Spec: "黑", PriceMajor: 299.0},
		},
	}

	got, err := orderflow.NewA2UIEmitter().Emit(c)
	if err != nil {
		t.Fatalf("Emit 失败：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("期望 3 报文，实际 %d", len(got))
	}

	// 第 0 条：createSurface
	if got[0]["action"] != runevent.A2UIActionCreateSurface {
		t.Errorf("第 0 条 action = %v", got[0]["action"])
	}
	if got[0]["catalogId"] != runevent.A2UICatalogID {
		t.Errorf("catalogId 不恒等")
	}

	// 第 1 条：updateComponents
	if got[1]["action"] != runevent.A2UIActionUpdateComponents {
		t.Errorf("第 1 条 action = %v", got[1]["action"])
	}

	// 第 2 条：updateDataModel
	if got[2]["action"] != runevent.A2UIActionUpdateDataModel {
		t.Errorf("第 2 条 action = %v", got[2]["action"])
	}
	if v2, ok := got[2]["value"].(map[string]any); !ok || v2["path"] != runevent.ShoppingRequirementsPath {
		t.Errorf("updateDataModel.value.path 不正确：%v", got[2]["value"])
	}
}

// TestA2UIEmit_NoItemsStillEmits 验证：空 items 时仍输出三报文（updateComponents 为空数组）。
func TestA2UIEmit_NoItemsStillEmits(t *testing.T) {
	c := orderflow.ConfirmationLike{ConfirmationID: "c1", Currency: "CNY"}

	got, err := orderflow.NewA2UIEmitter().Emit(c)
	if err != nil {
		t.Fatalf("Emit 失败：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("期望 3 报文，实际 %d", len(got))
	}
	comps, ok := got[1]["components"].([]any)
	if !ok || len(comps) != 0 {
		t.Errorf("updateComponents 应为空数组，实际 %v", got[1]["components"])
	}
}

// TestA2UIEmitter_BuildItemComponents_Structure 验证 buildItemComponents 输出正确的卡片结构。
func TestA2UIEmitter_BuildItemComponents_Structure(t *testing.T) {
	c := orderflow.ConfirmationLike{
		ConfirmationID: "c1",
		OperationID:    "op1",
		BuyerID:        "buyer1",
		SessionID:      "s1",
		Status:         "pending",
		ExpiresAt:      "2026-09-30T12:10:00Z",
		SubtotalMinor:  29900,
		Currency:       "CNY",
		SnapshotHash:   "abc",
		Items: []orderflow.ConfirmationItem{
			{ProductID: "P1", SKUID: "SKU1", Title: "商品A", Spec: "规格A", PriceMajor: 99.9},
			{ProductID: "P2", SKUID: "SKU2", Title: "商品B", Spec: "规格B", PriceMajor: 199.0},
		},
	}

	got, err := orderflow.NewA2UIEmitter().Emit(c)
	if err != nil {
		t.Fatalf("Emit 失败：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("期望 3 报文，实际 %d", len(got))
	}

	comps, ok := got[1]["components"].([]any)
	if !ok || len(comps) != 2 {
		t.Fatalf("updateComponents 应有 2 个卡片，实际 %v", got[1]["components"])
	}

	card0, ok := comps[0].(map[string]any)
	if !ok {
		t.Fatal("第一个卡片应为 map")
	}
	if card0["id"] != "card-0" {
		t.Errorf("第一张卡片 id 应为 card-0，实际 %v", card0["id"])
	}
	if card0["type"] != "ProductCard" {
		t.Errorf("第一张卡片 type 应为 ProductCard，实际 %v", card0["type"])
	}
}

// TestA2UIEmit_根Column挂载全部卡片 钉住「卡片真的会被渲染」这个前提。
//
// 前端的 Renderer 只对 type=Column 的节点下降，且子节点来自 props.children
// 里的组件 id 列表。少了它，报文照样过所有校验、卡片组件也都在 components
// 里，但页面上只会出现一个「(空 Column)」——四层校验没有一层能发现。
func TestA2UIEmit_根Column挂载全部卡片(t *testing.T) {
	c := orderflow.ConfirmationLike{
		ConfirmationID: "c1", Currency: "CNY",
		Items: []orderflow.ConfirmationItem{
			{ProductID: "P1", Title: "商品A", PriceMajor: 1},
			{ProductID: "P2", Title: "商品B", PriceMajor: 2},
		},
	}

	got, err := orderflow.NewA2UIEmitter().Emit(c)
	if err != nil {
		t.Fatalf("Emit 失败：%v", err)
	}

	root := findComponent(t, got[0], "root")
	children, ok := root["props"].(map[string]any)["children"].([]any)
	if !ok {
		t.Fatalf("根组件没有 children 列表：%v", root["props"])
	}

	// 两张卡片 + 小计行都要挂上。
	want := []string{"card-0", "card-1", "subtotal-line"}
	if len(children) != len(want) {
		t.Fatalf("children = %v，期望 %v", children, want)
	}
	for i, w := range want {
		if children[i] != w {
			t.Errorf("children[%d] = %v，期望 %s", i, children[i], w)
		}
	}
}

// TestSearchEmitter_根Column挂载全部卡片 同上，检索结果那一路。
func TestSearchEmitter_根Column挂载全部卡片(t *testing.T) {
	got, err := orderflow.NewSearchEmitter().Emit(twoHits())
	if err != nil {
		t.Fatalf("Emit 失败：%v", err)
	}

	root := findComponent(t, got[0], "root")
	children, ok := root["props"].(map[string]any)["children"].([]any)
	if !ok {
		t.Fatalf("根组件没有 children 列表：%v", root["props"])
	}
	if len(children) != 2 || children[0] != "hit-0" || children[1] != "hit-1" {
		t.Errorf("children = %v，期望 [hit-0 hit-1]", children)
	}

	// children 里列的每个 id，都必须在某条报文里真的定义了组件——
	// 挂了一个不存在的 id，前端会静默跳过它。
	defined := map[string]bool{}
	for _, msg := range got {
		comps, _ := msg["components"].([]any)
		for _, raw := range comps {
			if m, ok := raw.(map[string]any); ok {
				defined[m["id"].(string)] = true
			}
		}
	}
	for _, cid := range children {
		if !defined[cid.(string)] {
			t.Errorf("children 里的 %v 没有对应的组件定义", cid)
		}
	}
}

// findComponent 在一条报文里按 id 找组件。
func findComponent(t *testing.T, msg map[string]any, id string) map[string]any {
	t.Helper()
	comps, _ := msg["components"].([]any)
	for _, raw := range comps {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("报文里找不到组件 %s：%v", id, comps)
	return nil
}

// TestMarshalPayload_ErrorPath 验证 json.Marshal 失败时返回错误。
func TestMarshalPayload_ErrorPath(t *testing.T) {
	// 构造一个无法序列化的值（循环引用）
	cycle := make(map[string]any)
	cycle["self"] = &cycle
	_, err := orderflow.MarshalPayload(cycle)
	if err == nil {
		t.Error("循环引用的 map 应返回 json.Marshal 错误")
	}
}

// TestMarshalPayload_RoundTrip 验证 MarshalPayload 的序列化/反序列化正确性。
func TestMarshalPayload_RoundTrip(t *testing.T) {
	v := map[string]any{
		"key": "value",
		"nested": map[string]any{
			"a": 1,
			"b": "text",
		},
	}

	raw, err := orderflow.MarshalPayload(v)
	if err != nil {
		t.Fatalf("MarshalPayload 失败：%v", err)
	}

	var decoded map[string]any
	err = json.Unmarshal(raw, &decoded)
	if err != nil {
		t.Fatalf("反序列化失败：%v", err)
	}
	if decoded["key"] != "value" {
		t.Errorf("key 值不对，got %v", decoded["key"])
	}
}
