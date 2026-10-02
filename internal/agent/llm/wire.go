package llm

import (
	"encoding/json"

	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
)

// 本文件是「协议中立类型 ↔ OpenAI 兼容线上格式」的翻译层。
//
// 分离出来的理由：线上格式是被外部端点钉死的（字段名、嵌套、null 与否），
// 而客户端逻辑（重试、超时、错误分类）与它无关。混在一起时，改一次解析
// 就要重读一遍重试逻辑。

// wireRequest 是 POST /chat/completions 的请求体。
type wireRequest struct {
	Model      string        `json:"model"`
	Messages   []wireMessage `json:"messages"`
	Tools      []wireTool    `json:"tools,omitempty"`
	ToolChoice string        `json:"tool_choice,omitempty"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireTool 是工具声明；type 恒为 "function"（这是该协议里唯一的工具类型）。
type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function wireToolCallFunction `json:"function"`
}

type wireToolCallFunction struct {
	Name string `json:"name"`
	// Arguments 是 JSON **字符串**（不是对象）——协议如此，需要二次解析。
	Arguments string `json:"arguments"`
}

// wireUsage 是 OpenAI 兼容族的 usage 块。
//
// 「兼容」在这里只到字段命名一级：同一族的三家供应商对缓存命中的叫法并不一致，
// 所以三种形态都要认，而不是只认其中一家然后对另外两家静默拿到 0：
//   - prompt_tokens_details.cached_tokens —— OpenAI / Qwen / MiniMax
//   - prompt_cache_hit_tokens（顶层）      —— DeepSeek
//   - completion_tokens_details.reasoning_tokens —— 思维链模型的输出细分
type wireUsage struct {
	PromptTokens         int64 `json:"prompt_tokens"`
	CompletionTokens     int64 `json:"completion_tokens"`
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			// Content 可能为 null（纯工具调用时），故用指针而非 string。
			Content   *string        `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Usage 用指针：缺失时保持 nil，与「用量为 0」区分开（见 protocol.Step.Usage）。
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// toWireRequest 把协议请求翻译成线上请求体。
func toWireRequest(model string, req protocol.Request) wireRequest {
	msgs := make([]wireMessage, 0, len(req.History)+1)
	if req.System != "" {
		msgs = append(msgs, wireMessage{Role: string(protocol.RoleSystem), Content: req.System})
	}
	for _, m := range req.History {
		wm := wireMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
		}
		for _, c := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
				ID:       c.ID,
				Type:     "function",
				Function: wireToolCallFunction{Name: c.Name, Arguments: string(c.Arguments)},
			})
		}
		msgs = append(msgs, wm)
	}

	out := wireRequest{Model: model, Messages: msgs}
	if len(req.Tools) > 0 {
		out.Tools = make([]wireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			params := t.Parameters
			if len(params) == 0 {
				// 无参工具也要给一个合法的 object schema：省略 parameters
				// 会让部分供应商直接 400，而不是当成「没有参数」。
				params = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			out.Tools = append(out.Tools, wireTool{
				Type: "function",
				Function: wireFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  params,
				},
			})
		}
		// auto 表示「由模型自己决定调不调」；不设则部分端点默认不调工具。
		out.ToolChoice = "auto"
	}
	return out
}

// parseWireResponse 把线上响应翻译成协议响应。
func parseWireResponse(raw []byte) (protocol.Response, error) {
	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return protocol.Response{}, err
	}
	if wr.Error != nil {
		// 上游把业务错误塞在 200 里返回时的兜底。
		return protocol.Response{}, &StatusError{Status: 200, Body: wr.Error.Message}
	}
	if len(wr.Choices) == 0 {
		return protocol.Response{}, errNoChoices
	}

	ch := wr.Choices[0]
	content := ""
	if ch.Message.Content != nil {
		content = *ch.Message.Content
	}

	var calls []protocol.ToolCall
	for _, tc := range ch.Message.ToolCalls {
		args := json.RawMessage(tc.Function.Arguments)
		if len(args) == 0 || !json.Valid(args) {
			// 个别供应商对无参工具返回空串或 null。原样透传会让下游解析失败，
			// 归一成空对象：调用方拿到的是「没有参数」，而不是一次解析异常。
			args = json.RawMessage(`{}`)
		}
		calls = append(calls, protocol.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: args,
		})
	}

	return protocol.Response{Step: protocol.Step{
		Content: content,
		Calls:   calls,
		Usage:   parseUsage(wr.Usage),
	}}, nil
}

// parseUsage 把线上 usage 翻译成协议用量；上游没给就返回 nil。
//
// 不返回 error：usage 缺失或字段不全不是失败，只是「这次调用无法定价」，
// 由下游显式标成 unpriced。为它中断一次已经成功的模型调用是本末倒置。
func parseUsage(wu *wireUsage) *protocol.Usage {
	if wu == nil {
		return nil
	}
	cached := wu.PromptTokensDetails.CachedTokens
	if cached == 0 {
		// DeepSeek 走顶层字段名；两家用同一个语义、不同的键。
		cached = wu.PromptCacheHitTokens
	}
	return &protocol.Usage{
		InputTokens:  wu.PromptTokens,
		OutputTokens: wu.CompletionTokens,
		CachedTokens: cached,
		// reasoning 是 output 的细分，落库留痕但不参与计费（见 protocol.Usage）。
		ReasoningTokens: wu.CompletionTokensDetails.ReasoningTokens,
	}
}
