package tools

import (
	"encoding/json"
	"strings"
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
			Content: json.RawMessage(`{"hits": []}`),
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

func TestRegistry_Get_NotFound(t *testing.T) {
	reg := NewToolRegistry()
	_, ok := reg.Get("nonexistent_tool")
	if ok {
		t.Error("Get nonexistent_tool 应返回 false")
	}
}

func TestRegistry_All(t *testing.T) {
	reg := NewToolRegistry()
	defs := reg.All()
	if len(defs) != 14 {
		t.Errorf("All() 返回 %d 个工具，期望 14", len(defs))
	}
}

func TestValidateResult_ErrorBranch(t *testing.T) {
	reg := NewToolRegistry()

	// unknown tool -> nil (skipped)
	err := reg.ValidateResult("unknown_tool", ToolResult{State: ResultStateSuccess})
	if err != nil {
		t.Errorf("unknown tool 应跳过验证: %v", err)
	}

	// error state skips validation
	result := ToolResult{
		State: ResultStateError,
		Error: "something failed",
	}
	err = reg.ValidateResult("product_search_tool", result)
	if err != nil {
		t.Errorf("error state 应跳过验证: %v", err)
	}

	// success but missing required field
	result = ToolResult{
		State:   ResultStateSuccess,
		Content: json.RawMessage(`{}`),
	}
	err = reg.ValidateResult("product_search_tool", result)
	if err == nil {
		t.Error("缺少 required 字段应报错")
	}
	if !strings.Contains(err.Error(), "missing required field") {
		t.Errorf("错误信息 = %q，期望含 'missing required field'", err.Error())
	}

	// success with invalid JSON
	result = ToolResult{
		State:   ResultStateSuccess,
		Content: json.RawMessage(`{not json`),
	}
	err = reg.ValidateResult("product_search_tool", result)
	if err == nil {
		t.Error("非法 JSON 应报错")
	}
	if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("错误信息 = %q，期望含 'not valid JSON'", err.Error())
	}
}

func TestValidateSchema_InvalidBranches(t *testing.T) {
	reg := NewToolRegistry()

	t.Run("unknown tool", func(t *testing.T) {
		err := reg.ValidateSchema("nonexistent_tool")
		if err == nil {
			t.Error("unknown tool 应报错")
		}
		if !strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("错误信息 = %q，期望含 'unknown tool'", err.Error())
		}
	})

	t.Run("parameters type not object", func(t *testing.T) {
		reg2 := &ToolRegistry{defs: map[string]ToolDef{
			"bad_tool": {
				Name:        "bad_tool",
				Description: "test",
				Parameters: ToolParameters{
					Type:       "string",
					Properties: map[string]ParameterDef{},
				},
			},
		}}
		err := reg2.ValidateSchema("bad_tool")
		if err == nil {
			t.Error("type != 'object' 应报错")
		}
		if !strings.Contains(err.Error(), "type must be 'object'") {
			t.Errorf("错误信息 = %q，期望含 'type must be \\'object\\''", err.Error())
		}
	})

	t.Run("parameter missing type", func(t *testing.T) {
		reg2 := &ToolRegistry{defs: map[string]ToolDef{
			"bad_tool": {
				Name:        "bad_tool",
				Description: "test",
				Parameters: ToolParameters{
					Type: "object",
					Properties: map[string]ParameterDef{
						"p1": {Description: "no type"},
					},
				},
			},
		}}
		err := reg2.ValidateSchema("bad_tool")
		if err == nil {
			t.Error("缺少 type 应报错")
		}
		if !strings.Contains(err.Error(), "has no type") {
			t.Errorf("错误信息 = %q，期望含 'has no type'", err.Error())
		}
	})

	t.Run("array missing items", func(t *testing.T) {
		reg2 := &ToolRegistry{defs: map[string]ToolDef{
			"bad_tool": {
				Name:        "bad_tool",
				Description: "test",
				Parameters: ToolParameters{
					Type: "object",
					Properties: map[string]ParameterDef{
						"p1": {Type: "array"},
					},
				},
			},
		}}
		err := reg2.ValidateSchema("bad_tool")
		if err == nil {
			t.Error("array 缺少 items 应报错")
		}
		if !strings.Contains(err.Error(), "missing items schema") {
			t.Errorf("错误信息 = %q，期望含 'missing items schema'", err.Error())
		}
	})

	t.Run("object missing properties and enum", func(t *testing.T) {
		reg2 := &ToolRegistry{defs: map[string]ToolDef{
			"bad_tool": {
				Name:        "bad_tool",
				Description: "test",
				Parameters: ToolParameters{
					Type: "object",
					Properties: map[string]ParameterDef{
						"p1": {Type: "object"},
					},
				},
			},
		}}
		err := reg2.ValidateSchema("bad_tool")
		if err == nil {
			t.Error("object 缺少 properties 和 enum 应报错")
		}
		if !strings.Contains(err.Error(), "has no properties or enum") {
			t.Errorf("错误信息 = %q，期望含 'has no properties or enum'", err.Error())
		}
	})

	t.Run("required field not in properties", func(t *testing.T) {
		reg2 := &ToolRegistry{defs: map[string]ToolDef{
			"bad_tool": {
				Name:        "bad_tool",
				Description: "test",
				Parameters: ToolParameters{
					Type: "object",
					Properties: map[string]ParameterDef{
						"p1": {Type: "string"},
					},
					Required: []string{"nonexistent"},
				},
			},
		}}
		err := reg2.ValidateSchema("bad_tool")
		if err == nil {
			t.Error("required 字段不在 properties 应报错")
		}
		if !strings.Contains(err.Error(), "required parameter") {
			t.Errorf("错误信息 = %q，期望含 'required parameter'", err.Error())
		}
	})
}
