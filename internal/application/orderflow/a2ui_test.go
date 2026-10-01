package orderflow_test

import (
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
