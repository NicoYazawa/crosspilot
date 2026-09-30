package tools

import (
	"encoding/json"
	"testing"
)

func TestToolRegistryValidateAllSchemas(t *testing.T) {
	reg := NewToolRegistry()
	errs := reg.ValidateAllSchemas()
	if len(errs) > 0 {
		for _, err := range errs {
			t.Errorf("schema validation error: %v", err)
		}
	}
}

func TestToolRegistryValidateResult(t *testing.T) {
	reg := NewToolRegistry()

	t.Run("product_search_tool missing required field", func(t *testing.T) {
		result := ToolResult{
			Content: json.RawMessage(`{"hits": []}`), // missing recall_strategy
			State:   ResultStateSuccess,
		}
		err := reg.ValidateResult("product_search_tool", result)
		if err == nil {
			t.Error("expected validation error for missing recall_strategy")
		}
	})

	t.Run("product_search_tool valid", func(t *testing.T) {
		result := ToolResult{
			Content: json.RawMessage(`{"hits": [], "recall_strategy": "embedding_only"}`),
			State:   ResultStateSuccess,
		}
		err := reg.ValidateResult("product_search_tool", result)
		if err != nil {
			t.Errorf("unexpected validation error: %v", err)
		}
	})

	t.Run("error result skips validation", func(t *testing.T) {
		result := ToolResult{
			Content: json.RawMessage(`{}`),
			State:   ResultStateError,
			Error:   "something failed",
		}
		err := reg.ValidateResult("product_search_tool", result)
		if err != nil {
			t.Errorf("error state should skip validation: %v", err)
		}
	})
}

func TestToolRegistryListSchemas(t *testing.T) {
	reg := NewToolRegistry()
	schemas, err := reg.ListSchemasJSON()
	if err != nil {
		t.Fatalf("ListSchemasJSON failed: %v", err)
	}

	var parsed []map[string]any
	if err := json.Unmarshal(schemas, &parsed); err != nil {
		t.Fatalf("schemas is not valid JSON: %v", err)
	}

	if len(parsed) != 14 {
		t.Errorf("expected 14 tool schemas, got %d", len(parsed))
	}
}

func TestAllToolDefsComplete(t *testing.T) {
	defs := AllToolDefs()
	expectedNames := []string{
		"product_search_tool",
		"category_insight_tool",
		"conversation_fact_lookup_tool",
		"remember_preference_tool",
		"update_preference_tool",
		"forget_preference_tool",
		"create_order_tool",
		"query_order_tool",
		"cancel_order_tool",
		"task_dispatch_tool",
		"shopping_form_tool",
		"load_agent_skill_tool",
		"lookup_strategy_memory_tool",
		"web_search_tool",
	}
	if len(defs) != len(expectedNames) {
		t.Errorf("expected %d tools, got %d", len(expectedNames), len(defs))
	}
	names := make(map[string]bool)
	for _, def := range defs {
		names[def.Name] = true
	}
	for _, name := range expectedNames {
		if !names[name] {
			t.Errorf("missing tool: %s", name)
		}
	}
}
