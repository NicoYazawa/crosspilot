// Package protocol 定义 agent 与模型之间的协议中立类型。
//
// 为什么单独成包，而不是让 orchestrator 继续用 fakemodel 的类型：fakemodel 的
// 包注释自称「测试用的确定性 fake LLM」，生产接口却返回它的类型，等于把测试
// 夹具当成了协议。协议是编排层、真实适配器与 fake 三方共用的东西，谁都不该
// 依赖谁——本包是叶子，只依赖标准库。
package protocol

import (
	"encoding/json"
	"errors"
)

// Role 是对话消息的角色。
//
// 取值与 OpenAI 兼容协议一致（system/user/assistant/tool），因为目前所有
// 适配器都归一到这个族；换协议族时映射发生在适配器内部，不影响到这里。
type Role string

// 消息角色。system 只出现在请求首条，tool 只用于回填工具结果。
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolCall 是模型发起的一次工具调用。
//
// ID 从响应里原样带回：回填 tool 消息时必须以同一个 ID 关联。并行工具调用下
// 若丢了 ID，模型无法判断哪条结果对应哪次调用，只能瞎猜。
type ToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Usage 是一次模型调用的 token 计量。
//
// 两个「子集」关系是本结构最重要的语义，写错了会直接导致成本算错：
//   - CachedTokens **包含在** InputTokens 里（OpenAI 兼容族的 prompt_tokens
//     是总量，prompt_tokens_details.cached_tokens 是其中命中前缀缓存的那部分；
//     DeepSeek 同理，prompt_cache_hit_tokens + miss = prompt_tokens）。
//     计费时必须按 input - cached 算全价部分，否则缓存命中的 token 被收两次钱。
//   - ReasoningTokens **包含在** OutputTokens 里（completion_tokens 是总量）。
//     因此不单独计费，只落库留痕。
type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// Step 是模型的一轮输出。
type Step struct {
	// Content 是这一轮返回的文本。与工具调用可以同时出现（有的模型边解释边调工具）。
	Content string `json:"content"`
	// Calls 是这一轮发起的工具调用，按顺序串行执行。
	Calls []ToolCall `json:"calls"`
	// Err 表示这一轮以错误返回。
	//
	// 只有 fake model 会设置它——脚本需要能声明「这一轮应当失败」以穷举错误分支。
	// 真实适配器永远不设置：失败通过 Next 的 error 返回值表达。留着这个字段是为了
	// 让 fakemodel 能整体别名到本包（见 fakemodel.Step = protocol.Step），
	// 代价是一个真实适配器不会用到的字段。
	Err error `json:"-"`
	// Usage 是本轮的 token 用量。nil 表示上游没有返回 usage 字段——「不知道用了多少」
	// 与「用了 0 个」是两件事，nil 保留了这个区别，下游据此显式标 unpriced。
	//
	// 同上，放在 Step 而不是 Response 上，是为了让 fakemodel 的脚本能直接声明用量。
	Usage *Usage `json:"usage,omitempty"`
	// Provider / Model 是本次响应的来源标识，由适配器回填，供成本归因查价格表。
	//
	// 跟着每一次响应走，而不是在装配根烘成常量：网关出现 failover 或 A/B 换模型时，
	// 装配期常量会把成本记到没真正回答的那家名下。
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Response 是一次完整的模型回合。
type Response struct {
	Step
}

// Message 是发给模型的一条对话消息，覆盖四种角色。
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content,omitempty"`
	// ToolCalls 仅 assistant 消息使用：回放模型上一轮发起了哪些调用。
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID 仅 tool 消息使用：这条结果对应哪次调用。
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolDef 是「模型可以调用哪些工具」的一份声明。
//
// 刻意不复用 tools.ToolDef：那会把本包从叶子变成 tools 的依赖方，而适配器
// 只需要「名字 + 描述 + JSON Schema」这三样。转换发生在装配根。
type ToolDef struct {
	Name        string
	Description string
	// Parameters 是 JSON Schema 的 object 片段（type/properties/required）。
	Parameters json.RawMessage
}

// Request 是一次模型调用的全部输入。
type Request struct {
	// System 是系统提示词；空表示不加 system 消息。
	System string
	// History 是按时间排列的对话历史，最后一条是当前要回应的输入。
	History []Message
	// Tools 声明模型可用的工具；空表示这一轮不允许它调用工具。
	Tools []ToolDef
}

// ErrModelUnavailable 表示本次部署没有可用的模型网关。
//
// 定义成 sentinel 而不是普通 error：调用方要据此把「能力未接入」与「这次调用
// 失败了」分开。前者回 503（去开配置，重试无用），后者回 500。混成一个会让
// 运维拿着一条 500 去排查一个根本不存在的故障。
var ErrModelUnavailable = errors.New("模型网关尚未接入")
