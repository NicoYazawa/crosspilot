// Package orchestrator 提供 agent 的 ReAct 循环。
//
// 核心循环：模型决策 → 工具调用 → 注入结果 → 模型再决策 …
//
// 验收 D1：模型决策 + 工具调用完整跑通且落库；模型在确认无工具调用场景后退出循环。
//
// 与 tRPC-Agent-Go 的关系：tRPC-Agent-Go 提供模型适配层（failover/hedge/anthropic）
// 与 SSE 服务层；本包负责 ReAct 控制流，与具体 LLM 协议解耦——
// 接入时只需把 DecisionProvider 适配成 tRPC-Agent-Go 的 EventStreamReader 即可。
package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/agent/tools"
)

// DecisionProvider abstracts "the LLM". One method per turn.
//
// 传入完整 Request（系统提示词 + 对话历史 + 工具声明）而不是裸 prompt 字符串：
// 模型要能发起工具调用，就必须先知道有哪些工具、以及上一轮工具返回了什么。
// ctx 一路传到 HTTP 请求，取消 run 时才能真正中断推理。
type DecisionProvider interface {
	Next(ctx context.Context, req protocol.Request) (protocol.Response, error)
}

// Executor abstracts tool invocation (no direct dependency on tools.Executor
// keeps orchestrator testable with hand-rolled fakes).
type Executor interface {
	Execute(ctx context.Context, name string, args json.RawMessage) (tools.ToolResult, error)
}

// Event is one observable step emitted by the orchestrator.
//
// 调用方通过 chan<-Event 流式消费，用于 SSE 与日志。
type Event struct {
	// Kind is the event type.
	Kind string `json:"kind"`
	// Agent identifies the layer producing the event.
	Agent string `json:"agent"`
	// Content is the human-readable payload.
	Content string `json:"content,omitempty"`
	// ToolName is set for tool_call/tool_result events.
	ToolName string `json:"tool_name,omitempty"`
	// Iteration is the ReAct loop index (0-based).
	Iteration int `json:"iteration"`
	// Err is set for error events.
	Err error `json:"-"`
	// Usage / Provider / Model 只对 model_turn 事件有值，来自模型这一轮的响应。
	//
	// 编排层不解释它们，只做转手：成本归因需要知道「谁、用了多少 token」，
	// 而这两件事只有模型适配器知道。nil Usage 表示上游没返回用量。
	Usage    *protocol.Usage `json:"usage,omitempty"`
	Provider string          `json:"provider,omitempty"`
	Model    string          `json:"model,omitempty"`
}

// Event kinds emitted by the orchestrator.
const (
	KindRunStart    = "run_start"
	KindModelTurn   = "model_turn"
	KindToolCall    = "tool_call"
	KindToolResult  = "tool_result"
	KindRunFinished = "run_finished"
	KindRunError    = "run_error"
)

// Config is the orchestrator's per-run config.
type Config struct {
	// BuyerID identifies the buyer.
	SessionID string
	// Query is the buyer's natural-language request.
	Query string
	// Agent identifies the current agent layer; used for events.
	Agent string
	// MaxIterations caps the ReAct loop to avoid runaway.
	MaxIterations int
	// OnEvent is the streaming sink; nil means no streaming.
	OnEvent func(Event)
}

// Result 是一次 Run 的产物：按顺序记录的每一轮转录，以及最终模型文本。
//
// 模型决策序列、工具调用序列全部由 DecisionProvider 提供，这里只如实汇总：
// Transcript 按顺序保存每轮的模型内容、工具调用与工具结果；FinalText 是模型
// 宣告不再发起工具调用时给出的最终文本。
type Result struct {
	Transcript []Turn
	FinalText  string
}

// Turn is one round: model decision + tool calls + tool results.
type Turn struct {
	Content     string
	ToolCalls   []tools.CallRequest
	ToolResults []tools.ToolResult
}

// Orchestrator is the ReAct loop runner.
type Orchestrator struct {
	Model    DecisionProvider
	Executor Executor
	// SystemPrompt 是本次装配注入的系统提示词；空表示不发 system 消息。
	SystemPrompt string
	// Tools 是暴露给模型的工具声明。
	//
	// 只放真正注册了 handler 的工具：声明是模型对「自己能做什么」的唯一依据，
	// 把没有实现的工具写进去，模型只会反复调用，然后每一步都拿到 unknown tool。
	Tools []protocol.ToolDef
}

// New creates an Orchestrator with the given model and tool executor.
func New(model DecisionProvider, executor Executor) *Orchestrator {
	return &Orchestrator{Model: model, Executor: executor}
}

// Run executes the ReAct loop.
func (o *Orchestrator) Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 8
	}
	emit(cfg.OnEvent, Event{Kind: KindRunStart, Agent: cfg.Agent, Content: cfg.Query, Iteration: 0})

	result := Result{}
	// history 是喂给模型的真实对话历史：用户输入打头，之后每轮追加一条 assistant
	// 消息与若干条 tool 消息。这取代了此前「把 Turn 序列化成 JSON 拼在 prompt 后面」
	// 的简化实现——那种拼法里模型看不到角色边界，也就无法把工具结果与自己的
	// 调用对应起来。
	history := []protocol.Message{{Role: protocol.RoleUser, Content: cfg.Query}}

	for i := 0; i < cfg.MaxIterations; i++ {
		resp, err := o.Model.Next(ctx, protocol.Request{
			System:  o.SystemPrompt,
			History: history,
			Tools:   o.Tools,
		})
		if err != nil {
			emit(cfg.OnEvent, Event{Kind: KindRunError, Agent: cfg.Agent, Err: err, Iteration: i})
			return result, err
		}

		emit(cfg.OnEvent, Event{
			Kind: KindModelTurn, Agent: cfg.Agent, Content: resp.Content, Iteration: i,
			Usage: resp.Usage, Provider: resp.Provider, Model: resp.Model,
		})

		turn := Turn{Content: resp.Content}
		for _, c := range resp.Calls {
			turn.ToolCalls = append(turn.ToolCalls, tools.CallRequest{
				Name:      c.Name,
				Arguments: c.Arguments,
			})
		}

		if len(resp.Calls) == 0 {
			// 模型未发起工具调用即视为结束。
			result.Transcript = append(result.Transcript, turn)
			result.FinalText = resp.Content
			break
		}

		// 先把这一轮的 assistant 消息（含它发起的调用）记进历史，
		// 再逐条追加对应的 tool 结果——顺序反了模型就看不到「调用 → 结果」的因果。
		history = append(history, assistantMessage(resp, i))

		// 串行执行所有工具调用（顺序保留）。
		for idx, call := range resp.Calls {
			emit(cfg.OnEvent, Event{
				Kind: KindToolCall, Agent: cfg.Agent, ToolName: call.Name, Iteration: i,
				Content: string(call.Arguments),
			})

			toolResult, execErr := o.Executor.Execute(ctx, call.Name, call.Arguments)
			if execErr != nil && !errors.Is(execErr, tools.ErrUnknownTool) {
				// 注册的工具未注册是预期内的失败（模型误调用）；其他错误向上抛。
				emit(cfg.OnEvent, Event{Kind: KindRunError, Agent: cfg.Agent, Err: execErr, Iteration: i})
				return result, execErr
			}

			if toolResult.State == "" {
				toolResult.State = tools.ResultStateSuccess
			}
			turn.ToolResults = append(turn.ToolResults, toolResult)

			emit(cfg.OnEvent, Event{
				Kind: KindToolResult, Agent: cfg.Agent, ToolName: call.Name, Iteration: i,
				Content: summary(toolResult),
			})

			history = append(history, protocol.Message{
				Role:       protocol.RoleTool,
				ToolCallID: toolCallID(call, i, idx),
				Content:    summary(toolResult),
			})
		}

		result.Transcript = append(result.Transcript, turn)
	}

	emit(cfg.OnEvent, Event{Kind: KindRunFinished, Agent: cfg.Agent, Iteration: cfg.MaxIterations})
	return result, nil
}

// assistantMessage 把模型这一轮的输出转成一条 assistant 消息，供下一轮回放。
//
// 调用 ID 必须走 toolCallID 而不是直接用 c.ID：紧随其后的 tool 消息用的是
// 合成后的 ID，这里若原样回放模型给的空 ID，两个 ID 就对不上——OpenAI 兼容
// 端点会以 400 拒掉整个请求，且报错信息只说「tool_call_id 不存在」，很难
// 让人联想到是这里发散的。
func assistantMessage(resp protocol.Response, iteration int) protocol.Message {
	msg := protocol.Message{Role: protocol.RoleAssistant, Content: resp.Content}
	for idx, c := range resp.Calls {
		msg.ToolCalls = append(msg.ToolCalls, protocol.ToolCall{
			ID: toolCallID(c, iteration, idx), Name: c.Name, Arguments: c.Arguments,
		})
	}
	return msg
}

// toolCallID 返回这次调用的 ID，模型没给时合成一个稳定值。
//
// OpenAI 兼容响应总会带 id，但个别供应商与 fake 可能省略，而 tool 消息的
// tool_call_id 是必填的：缺了它模型分不清结果属于哪次调用。留空串会让下一轮
// 请求被上游直接拒掉，宁可合成一个。
func toolCallID(call protocol.ToolCall, iteration, index int) string {
	if call.ID != "" {
		return call.ID
	}
	return fmt.Sprintf("call_%d_%d", iteration, index)
}

func emit(onEvent func(Event), ev Event) {
	if onEvent != nil {
		onEvent(ev)
	}
}

func summary(r tools.ToolResult) string {
	if r.State == tools.ResultStateError {
		return "error: " + r.Error
	}
	if len(r.Content) == 0 {
		return ""
	}
	return string(r.Content)
}
