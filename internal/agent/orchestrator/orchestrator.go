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

	"github.com/NicoYazawa/crosspilot/internal/agent/fakemodel"
	"github.com/NicoYazawa/crosspilot/internal/agent/tools"
)

// DecisionProvider abstracts "the LLM". One method per turn.
type DecisionProvider interface {
	Next(prompt string) (fakemodel.Response, error)
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

// Run executes the ReAct loop until the model emits no more tool calls
// or MaxIterations is reached.
//
// 模型决策序列、工具调用序列全部由 DecisionProvider 提供。
// Run 返回：调用过的所有工具调用结果（按顺序）+ 最终模型文本。
type Result struct {
	Transcript []Turn
	FinalText  string
}

// Turn is one round: model decision + tool calls + tool results.
type Turn struct {
	Content    string
	ToolCalls  []tools.CallRequest
	ToolResults []tools.ToolResult
}

// Orchestrator is the ReAct loop runner.
type Orchestrator struct {
	Model    DecisionProvider
	Executor Executor
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
	prompt := cfg.Query

	for i := 0; i < cfg.MaxIterations; i++ {
		resp, err := o.Model.Next(prompt)
		if err != nil {
			emit(cfg.OnEvent, Event{Kind: KindRunError, Agent: cfg.Agent, Err: err, Iteration: i})
			return result, err
		}

		emit(cfg.OnEvent, Event{Kind: KindModelTurn, Agent: cfg.Agent, Content: resp.Content, Iteration: i})

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

		// 串行执行所有工具调用（顺序保留）。
		for _, call := range resp.Calls {
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
		}

		result.Transcript = append(result.Transcript, turn)

		// 把工具结果拼回 prompt（简化版：用文本 JSON）。
		prompt = buildNextPrompt(prompt, turn)
	}

	emit(cfg.OnEvent, Event{Kind: KindRunFinished, Agent: cfg.Agent, Iteration: cfg.MaxIterations})
	return result, nil
}

// buildNextPrompt appends tool results to the running prompt so the model
// sees them on the next iteration.
//
// 简化实现：把 turn 序列化为 JSON 拼在 prompt 后面。
// 真实生产里这一步要按各家模型的对话格式重新构造（system/user/tool role）。
func buildNextPrompt(prev string, turn Turn) string {
	body, _ := json.Marshal(turn)
	return fmt.Sprintf("%s\n\n--- turn ---\n%s", prev, string(body))
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