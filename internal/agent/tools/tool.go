package tools

import (
	"encoding/json"
	"fmt"
)

// ToolDef is the JSON Schema definition of a tool.
// Each tool declares its parameters explicitly since Go cannot
// derive schemas from function signatures like Python can.
type ToolDef struct {
	Name        string
	Description string
	Parameters  ToolParameters
}

// ToolParameters is a JSON Schema object for tool parameters.
type ToolParameters struct {
	Type       string                  `json:"type"`
	Properties map[string]ParameterDef `json:"properties"`
	Required   []string                `json:"required"`
}

// ParameterDef is a single parameter schema.
// For object types, Properties and Required are used instead of Type/Enum/Items.
type ParameterDef struct {
	Type        string                  `json:"type"`
	Description string                  `json:"description"`
	Enum        []string                `json:"enum,omitempty"`
	Default     any                     `json:"default,omitempty"`
	Items       *ParameterDef           `json:"items,omitempty"`
	Properties  map[string]ParameterDef `json:"properties,omitempty"`
	Required    []string                `json:"required,omitempty"`
}

// ToolResult is what a tool returns to the agent.
type ToolResult struct {
	Content json.RawMessage `json:"content"`
	State   ResultState     `json:"state"`
	Error   string          `json:"error,omitempty"`
}

// ResultState is the state of a tool result.
type ResultState string

// ResultStateSuccess 表示工具调用成功；ResultStateError 表示工具返回了业务错误。
const (
	ResultStateSuccess ResultState = "success"
	ResultStateError   ResultState = "error"
)

// Validate checks that the tool result contains required fields.
func (r ToolResult) Validate(requiredFields []string) error {
	if r.State == ResultStateError {
		return nil // errors don't need content validation
	}
	var data map[string]any
	if err := json.Unmarshal(r.Content, &data); err != nil {
		return fmt.Errorf("tool result content is not valid JSON: %w", err)
	}
	for _, field := range requiredFields {
		if _, ok := data[field]; !ok {
			return fmt.Errorf("tool result missing required field: %s", field)
		}
	}
	return nil
}

// AllToolDefs returns the JSON Schema definitions for all 11 tools.
func AllToolDefs() []ToolDef {
	return []ToolDef{
		ProductSearchToolDef(),
		CategoryInsightToolDef(),
		ConversationFactLookupToolDef(),
		RememberPreferenceToolDef(),
		UpdatePreferenceToolDef(),
		ForgetPreferenceToolDef(),
		CreateOrderToolDef(),
		QueryOrderToolDef(),
		CancelOrderToolDef(),
		TaskDispatchToolDef(),
		ShoppingFormToolDef(),
		LoadAgentSkillToolDef(),
		LookupStrategyMemoryToolDef(),
		WebSearchToolDef(),
	}
}

// 工具名常量。
//
// 与 ToolRequiredFields 的键、工具 schema 的 Name、以及系统提示词里描述的名字
// 必须是同一个字符串。集中在这里是为了让「工具名」只有一个定义点：它在装配根
// （注册 handler）、在应用层（据 ToolName 决定发不发 A2UI 卡片）都要被引用，
// 三处各写一遍字面量，改一次名字就会漏掉两处。
const (
	// ToolProductSearch 是跨境商品检索工具。
	ToolProductSearch = "product_search_tool"
)

// ToolRequiredFields maps tool name → required result fields.
var ToolRequiredFields = map[string][]string{
	"product_search_tool":         {"hits", "recall_strategy"},
	"category_insight_tool":       {"insights"},
	"create_order_tool":           {"confirmation_required", "confirmation"},
	"query_order_tool":            {"order_id", "status"},
	"cancel_order_tool":           {"confirmation_required", "confirmation"},
	"task_dispatch_tool":          {"agent", "status"},
	"shopping_form_tool":          {"form_id", "status"},
	"load_agent_skill_tool":       {"content_hash"},
	"lookup_strategy_memory_tool": {"strategies"},
	"web_search_tool":             {"results"},
}
