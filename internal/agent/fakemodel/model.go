// Package fakemodel 提供 agent 测试用的确定性 fake LLM。
//
// 真实 LLM 不可控：相同 prompt 可能产生不同结果，CI 上同一段断言在本地
// 通过、生产却翻车。fake model 的目的就是把模型这一层也变成「可注入依赖」：
// 测试脚本式地声明每轮该返回什么（普通内容、工具调用、超时、错误等），
// agent 主体逻辑就能在不动模型的情况下被穷举。
package fakemodel

import (
	"encoding/json"
	"fmt"
	"sync"
)

// ToolCall 是 fake model 在某一轮应当发出的工具调用。
type ToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Step 是 fake model 的一轮输出。
type Step struct {
	// Content 是这一轮返回给用户的文本。空字符串也可以。
	Content string `json:"content"`
	// Calls 是这一轮要执行的工具调用，按顺序串行执行。
	Calls []ToolCall `json:"calls"`
	// Err 表示这一轮应当以错误返回。优先级最高（覆盖 Content/Calls）。
	Err error `json:"-"`
}

// Response is one full assistant turn from the fake model.
type Response struct {
	Step
}

// FakeModel implements a scriptable model for tests.
type FakeModel struct {
	mu      sync.Mutex
	steps   []Response
	current int
	// OnNext is called after every call; tests use it to inspect prompts/counters.
	OnNext func(prompt string) string
}

// New creates a FakeModel from a script of responses.
func New(steps ...Response) *FakeModel {
	return &FakeModel{steps: steps}
}

// Next advances to the next scripted response.
//
// Returns the next Response and a snapshot of how many steps remain (including current).
// If the script is exhausted, returns an error so the agent loop terminates.
func (f *FakeModel) Next(prompt string) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.OnNext != nil {
		f.OnNext(prompt)
	}

	if f.current >= len(f.steps) {
		return Response{}, fmt.Errorf("fakemodel: 脚本已耗尽（已使用 %d 步）", f.current)
	}
	resp := f.steps[f.current]
	f.current++
	if resp.Err != nil {
		return Response{}, resp.Err
	}
	return resp, nil
}

// CallsSoFar returns how many scripted steps have been consumed.
func (f *FakeModel) CallsSoFar() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

// Remaining returns how many scripted steps are still unconsumed.
func (f *FakeModel) Remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.steps) - f.current
}

// MustArgs parses tool call arguments; panics on malformed JSON so test scripts
// can stay terse.
func MustArgs(raw string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		panic("fakemodel: 参数 JSON 不合法：" + err.Error())
	}
	return m
}

// MustArgsRaw returns the raw JSON for passing directly to a tool.
func MustArgsRaw(raw string) json.RawMessage {
	if !json.Valid([]byte(raw)) {
		panic("fakemodel: 参数不是合法 JSON：" + raw)
	}
	return json.RawMessage(raw)
}

// ContextKey is the standard request key passed by the orchestrator.
type ContextKey string

const (
	// KeyPrompt is the user-facing prompt passed into Next().
	KeyPrompt ContextKey = "prompt"
)
