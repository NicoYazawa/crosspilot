package runevent_test

import (
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
)

// TestA2UIValidateCreateSurface 验收 E6：createSurface 报文契约。
func TestA2UIValidateCreateSurface(t *testing.T) {
	good := map[string]any{
		"action":    runevent.A2UIActionCreateSurface,
		"catalogId": runevent.A2UICatalogID,
		"version":   runevent.A2UIVersion,
		"components": []any{
			map[string]any{
				"id":   "root",
				"type": "Column",
				"path": runevent.ShoppingRequirementsPath,
			},
			map[string]any{
				"id":   "search-input",
				"type": "TextInput",
				"props": map[string]any{
					"placeholder": "搜索商品",
				},
			},
		},
	}
	if err := runevent.ValidateCreateSurface(good); err != nil {
		t.Fatalf("合规报文应通过：%v", err)
	}
}

// TestCases
func TestA2UIValidateCreateSurface_RejectsWrongCatalogID(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionCreateSurface,
		"catalogId": "wrong.catalog",
		"components": []any{
			map[string]any{"id": "root", "type": "Column", "path": runevent.ShoppingRequirementsPath},
		},
	}
	err := runevent.ValidateCreateSurface(bad)
	if err == nil {
		t.Fatal("错 catalogId 应被拒绝")
	}
}

// TestCases
func TestA2UIValidateCreateSurface_RejectsMissingRoot(t *testing.T) {
	bad := map[string]any{
		"action":    runevent.A2UIActionCreateSurface,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			map[string]any{"id": "leaf", "type": "Text"},
		},
	}
	err := runevent.ValidateCreateSurface(bad)
	if err == nil {
		t.Fatal("无 /requirements 根组件应被拒绝")
	}
}

func TestA2UIValidateCreateSurface_RejectsEmptyComponents(t *testing.T) {
	bad := map[string]any{
		"action":     runevent.A2UIActionCreateSurface,
		"catalogId":  runevent.A2UICatalogID,
		"components": []any{},
	}
	if err := runevent.ValidateCreateSurface(bad); err == nil {
		t.Fatal("空 components 应被拒绝")
	}
}

func TestA2UIValidateUpdateComponents(t *testing.T) {
	good := map[string]any{
		"action":    runevent.A2UIActionUpdateComponents,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			map[string]any{
				"id":      "card-001",
				"type":    "ProductCard",
				"surface": "dynamic",
				"props":   map[string]any{"title": "防水登山包"},
			},
		},
	}
	if err := runevent.ValidateUpdateComponents(good); err != nil {
		t.Fatalf("合规报文应通过：%v", err)
	}

	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateComponents,
		"catalogId": runevent.A2UICatalogID,
		"components": []any{
			map[string]any{"type": "ProductCard"},
		},
	}
	if err := runevent.ValidateUpdateComponents(bad); err == nil {
		t.Fatal("缺 id 应被拒绝")
	}
}

func TestA2UIValidateUpdateDataModel(t *testing.T) {
	good := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
		"value": map[string]any{
			"path": runevent.ShoppingRequirementsPath,
			"data": []any{
				map[string]any{"key": "items", "limit": []any{}},
			},
		},
	}
	if err := runevent.ValidateUpdateDataModel(good); err != nil {
		t.Fatalf("合规报文应通过：%v", err)
	}

	bad := map[string]any{
		"action":    runevent.A2UIActionUpdateDataModel,
		"catalogId": runevent.A2UICatalogID,
		"value":     map[string]any{"data": []any{}},
	}
	if err := runevent.ValidateUpdateDataModel(bad); err == nil {
		t.Fatal("缺 value.path 应被拒绝")
	}
}
