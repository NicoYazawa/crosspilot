// Package a2ui 实现 A2UI v0.9 报文契约。
//
// A2UI（Agent-to-UI）是用 JSON 描述「前端应当如何渲染当前对话」的协议。
// 我们用的是 v0.9：与 v0.8 不同的是用 createSurface / updateComponents /
// updateDataModel 三个独立报文，而不是 beginRendering + surfaceUpdate + dataModelUpdate
// 三个。两者不兼容，前端只能识别一套，本文件只暴露 v0.9。
//
// 关键不变量：
//   - CatalogId 永远是 "globex.local/shopping-v2"。
//   - createSurface 必须包含 root 组件（type="Column" / path="/requirements"）。
//   - updateComponents 必须包含 type 字段；非法 type 一律拒渲染。
//   - updateDataModel 必须包含 value.path 与 value.data。
package runevent

// A2UICatalogID 是购物场景的固定 catalogId。
//
// 源项目硬编码此值；前端据此选择组件渲染器。改这里就是改契约，必须同步改前端。
const A2UICatalogID = "globex.local/shopping-v2"

// A2UIVersion 是适配器版本字符串。前端拒渲染闸门会校验它。
const A2UIVersion = "0.9"

// A2UI 报文动作名。
//
// 三个选择必须按 createSurface → updateComponents → updateDataModel 顺序发出，
// 顺序错了前端会拒渲染。
const (
	A2UIActionCreateSurface     = "createSurface"
	A2UIActionUpdateComponents  = "updateComponents"
	A2UIActionUpdateDataModel   = "updateDataModel"
)

// ShoppingRequirementsPath 是 A2UI 数据模型的根路径。
//
// 整套购物对话的数据都挂在这一棵树上：商品候选、已选清单、确认单摘要都在这。
const ShoppingRequirementsPath = "/requirements"

// A2UIComponent 是一个可渲染单元。
//
// Type 必须是渲染器已注册的合法类型。Surface="dynamic" 表示组件由 updateDataModel
// 注入数据，Surface="static" 表示组件自身已包含全部数据。
type A2UIComponent struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	Path    string         `json:"path,omitempty"`
	Props   map[string]any `json:"props,omitempty"`
	Surface string         `json:"surface,omitempty"`
}

// A2UIDataEntry 是数据模型中的一项数据。
//
// 同一 Path 下可以挂多份数据：value.data 是数组，按顺序渲染。Value 是完整 v0.9 字段
// 集合的兼容视图——前端按 schema.list[0].fields 必填值比对。
type A2UIDataEntry struct {
	Path  string         `json:"path"`
	Data  []map[string]any `json:"data"`
}

// ValidateCreateSurface 校验 createSurface 报文。
//
// 不接受零值。返回错误时调用方应丢弃该报文而非尝试修复——前端拒渲染闸门要求
// 报文契约完全一致，修补过的反而更难排查。
func ValidateCreateSurface(payload map[string]any) error {
	if got := payload["action"]; got != A2UIActionCreateSurface {
		return &A2UIError{Field: "action", Want: A2UIActionCreateSurface, Got: asString(got)}
	}
	catalog, ok := payload["catalogId"].(string)
	if !ok || catalog != A2UICatalogID {
		return &A2UIError{Field: "catalogId", Want: A2UICatalogID, Got: catalog}
	}
	version, _ := payload["version"].(string)
	if version != "" && version != A2UIVersion {
		return &A2UIError{Field: "version", Want: A2UIVersion, Got: version}
	}
	components, ok := payload["components"].([]any)
	if !ok || len(components) == 0 {
		return &A2UIError{Field: "components", Want: "[]A2UIComponent (non-empty)", Got: "missing or empty"}
	}
	rootSeen := false
	for i, raw := range components {
		c, ok := raw.(map[string]any)
		if !ok {
			return &A2UIError{Field: "components[" + itoa(i) + "]", Want: "object", Got: "non-object"}
		}
		if typ, present := c["type"]; !present || typ == "" {
			return &A2UIError{Field: "components[" + itoa(i) + "].type", Want: "non-empty", Got: ""}
		}
		if path, present := c["path"]; present && path == ShoppingRequirementsPath {
			rootSeen = true
		}
	}
	if !rootSeen {
		return &A2UIError{Field: "components[?].path", Want: ShoppingRequirementsPath, Got: "missing"}
	}
	return nil
}

// ValidateUpdateComponents 校验 updateComponents 报文。
func ValidateUpdateComponents(payload map[string]any) error {
	if got := payload["action"]; got != A2UIActionUpdateComponents {
		return &A2UIError{Field: "action", Want: A2UIActionUpdateComponents, Got: asString(got)}
	}
	catalog, ok := payload["catalogId"].(string)
	if !ok || catalog != A2UICatalogID {
		return &A2UIError{Field: "catalogId", Want: A2UICatalogID, Got: catalog}
	}
	updates, ok := payload["components"].([]any)
	if !ok {
		return &A2UIError{Field: "components", Want: "[]A2UIComponent", Got: "missing"}
	}
	for i, raw := range updates {
		c, ok := raw.(map[string]any)
		if !ok {
			return &A2UIError{Field: "components[" + itoa(i) + "]", Want: "object", Got: "non-object"}
		}
		if id, present := c["id"]; !present || id == "" {
			return &A2UIError{Field: "components[" + itoa(i) + "].id", Want: "non-empty", Got: ""}
		}
		if c["type"] == "" {
			return &A2UIError{Field: "components[" + itoa(i) + "].type", Want: "non-empty", Got: ""}
		}
	}
	return nil
}

// ValidateUpdateDataModel 校验 updateDataModel 报文。
func ValidateUpdateDataModel(payload map[string]any) error {
	if got := payload["action"]; got != A2UIActionUpdateDataModel {
		return &A2UIError{Field: "action", Want: A2UIActionUpdateDataModel, Got: asString(got)}
	}
	catalog, ok := payload["catalogId"].(string)
	if !ok || catalog != A2UICatalogID {
		return &A2UIError{Field: "catalogId", Want: A2UICatalogID, Got: catalog}
	}
	value, ok := payload["value"].(map[string]any)
	if !ok {
		return &A2UIError{Field: "value", Want: "{path,data}", Got: "missing"}
	}
	path, ok := value["path"].(string)
	if !ok || path == "" {
		return &A2UIError{Field: "value.path", Want: "non-empty string", Got: ""}
	}
	if _, ok := value["data"]; !ok {
		return &A2UIError{Field: "value.data", Want: "array", Got: "missing"}
	}
	return nil
}

// A2UIError 是契约违反错误的详细描述。
//
// 故意做成结构体而不是 errors.New：前端拒渲染闸门在测试中断言「到底是哪个字段
// 不合规」，字符串拼接会让调试时一通乱猜。
type A2UIError struct {
	Field string
	Want  string
	Got   string
}

func (e *A2UIError) Error() string {
	return "a2ui: 字段 " + e.Field + " 应为 " + e.Want + "，实际 " + e.Got
}

func asString(v any) string {
	if v == nil {
		return "null"
	}
	if s, ok := v.(string); ok {
		return s
	}
	return "non-string"
}

// itoa 把 int 转字符串。go 1.27 的 strconv 也能，但内联减少一处 import。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}