package tools

import (
	"encoding/json"
	"fmt"
)

// ToolRegistry maintains all registered tools and their schemas.
type ToolRegistry struct {
	defs map[string]ToolDef
}

// NewToolRegistry creates a new tool registry with all 14 tool definitions.
func NewToolRegistry() *ToolRegistry {
	r := &ToolRegistry{defs: make(map[string]ToolDef)}
	for _, def := range AllToolDefs() {
		r.defs[def.Name] = def
	}
	return r
}

// Get returns a tool definition by name.
func (r *ToolRegistry) Get(name string) (ToolDef, bool) {
	def, ok := r.defs[name]
	return def, ok
}

// All returns all registered tool definitions.
func (r *ToolRegistry) All() []ToolDef {
	defs := make([]ToolDef, 0, len(r.defs))
	for _, def := range r.defs {
		defs = append(defs, def)
	}
	return defs
}

// ValidateResult checks that a tool result contains required fields.
func (r *ToolRegistry) ValidateResult(toolName string, result ToolResult) error {
	required, ok := ToolRequiredFields[toolName]
	if !ok {
		return nil // unknown tools skip validation
	}
	return result.Validate(required)
}

// ValidateSchema checks that a tool's JSON Schema is structurally valid.
func (r *ToolRegistry) ValidateSchema(toolName string) error {
	def, ok := r.defs[toolName]
	if !ok {
		return fmt.Errorf("unknown tool: %s", toolName)
	}
	if def.Parameters.Type != "object" {
		return fmt.Errorf("%s: parameters type must be 'object', got %s", toolName, def.Parameters.Type)
	}
	for paramName, param := range def.Parameters.Properties {
		if param.Type == "" {
			return fmt.Errorf("%s: parameter %q has no type", toolName, paramName)
		}
		if param.Type == "array" && param.Items == nil {
			return fmt.Errorf("%s: array parameter %q missing items schema", toolName, paramName)
		}
		if param.Type == "object" && param.Properties == nil && len(param.Enum) == 0 {
			return fmt.Errorf("%s: object parameter %q has no properties or enum", toolName, paramName)
		}
	}
	for _, req := range def.Parameters.Required {
		if _, ok := def.Parameters.Properties[req]; !ok {
			return fmt.Errorf("%s: required parameter %q not in properties", toolName, req)
		}
	}
	return nil
}

// ValidateAllSchemas runs schema validation on all registered tools.
func (r *ToolRegistry) ValidateAllSchemas() []error {
	var errs []error
	for name := range r.defs {
		if err := r.ValidateSchema(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// ToJSONSchema converts a ToolDef to a jsonschema-compatible map.
func (r *ToolRegistry) ToJSONSchema(name string) (map[string]any, error) {
	def, ok := r.defs[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	properties := make(map[string]any)
	for paramName, param := range def.Parameters.Properties {
		p := map[string]any{
			"description": param.Description,
		}
		if param.Type == "array" && param.Items != nil {
			p["type"] = "array"
			p["items"] = map[string]any{"type": param.Items.Type}
		} else if len(param.Enum) > 0 {
			p["type"] = "string"
			p["enum"] = param.Enum
		} else {
			p["type"] = param.Type
		}
		if param.Default != nil {
			p["default"] = param.Default
		}
		properties[paramName] = p
	}
	schema := map[string]any{
		"name":        def.Name,
		"description": def.Description,
		"parameters": map[string]any{
			"type":       "object",
			"properties": properties,
		},
	}
	if len(def.Parameters.Required) > 0 {
		schema["parameters"].(map[string]any)["required"] = def.Parameters.Required
	}
	return schema, nil
}

// ListSchemasJSON returns all tool schemas as JSON.
func (r *ToolRegistry) ListSchemasJSON() ([]byte, error) {
	schemas := make([]map[string]any, 0, len(r.defs))
	for name := range r.defs {
		schema, err := r.ToJSONSchema(name)
		if err != nil {
			return nil, err
		}
		schemas = append(schemas, schema)
	}
	return json.Marshal(schemas)
}
