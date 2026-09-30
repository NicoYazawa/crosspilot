package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Executor runs tool calls against registered handlers.
//
// Tool 注册时按名字映射到一个 func(ctx, argsJSON) (ToolResult, error)。
// 测试时可注入任意 fake handler；生产时由具体应用层提供真实 handler。
type Executor struct {
	mu       sync.RWMutex
	handlers map[string]ToolHandler
}

// ToolHandler is the implementation behind a tool name.
//
// 返回的 ToolResult.Content 是 JSON 字符串；调用方负责保证它能被 schema 校验。
type ToolHandler func(ctx context.Context, args json.RawMessage) (ToolResult, error)

// NewExecutor creates an empty Executor.
func NewExecutor() *Executor {
	return &Executor{handlers: make(map[string]ToolHandler)}
}

// Register binds a handler to a tool name.
//
// 重复注册同名的 handler 会 panic：与 ToolRequiredFields 白名单的契约一致，
// 不允许同一工具对应多份语义不同的实现。
func (e *Executor) Register(name string, h ToolHandler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.handlers[name]; exists {
		panic("tools: 重复注册 handler " + name)
	}
	e.handlers[name] = h
}

// Has reports whether s is registered.
func (e *Executor) Has(name string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	_, ok := e.handlers[name]
	return ok
}

// Execute invokes a registered tool by name.
//
// 未注册的工具名返回 ErrUnknownTool，而不是 panic——orchestrator 应当捕获
// 这种情况并向 LLM 报告，而不是让整个 ReAct 崩掉。
func (e *Executor) Execute(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
	e.mu.RLock()
	h, ok := e.handlers[name]
	e.mu.RUnlock()
	if !ok {
		return ToolResult{
			State: ResultStateError,
			Error: fmt.Sprintf("unknown tool: %s", name),
		}, ErrUnknownTool
	}
	return h(ctx, args)
}

// ErrUnknownTool indicates the tool name has no registered handler.
var ErrUnknownTool = errors.New("tools: 未注册的工具")

// CallRequest 是 orchestrator 发出的工具调用请求。
type CallRequest struct {
	Name      string
	Arguments json.RawMessage
}

// DecodeArgs parses JSON arguments into a typed struct.
//
// 测试时常用。
func DecodeArgs[T any](raw json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, err
	}
	return v, nil
}