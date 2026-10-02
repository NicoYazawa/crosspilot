// Package fakemodel 提供 agent 测试用的确定性 fake LLM。
//
// 真实 LLM 不可控：相同 prompt 可能产生不同结果，CI 上同一段断言在本地
// 通过、生产却翻车。fake model 的目的就是把模型这一层也变成「可注入依赖」：
// 测试脚本式地声明每轮该返回什么（普通内容、工具调用、超时、错误等），
// agent 主体逻辑就能在不动模型的情况下被穷举。
package fakemodel

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
)

// 下面三个类型是协议类型的别名，不是各自定义。
//
// 用别名而不是定义新类型：协议类型原本就住在本包，后来搬去了 protocol 包
// （生产接口不该返回测试夹具的类型）。别名让既有测试里几十处
// `fakemodel.Response{...}` / `fakemodel.ToolCall{...}` 一个字都不用改，
// 同时它们现在共享的正是适配器与编排层使用的那套类型——测试构造的响应，
// 与真实适配器返回的响应，是同一个结构体。
type (
	// ToolCall 是模型发起的一次工具调用。
	ToolCall = protocol.ToolCall

	// Step 是模型的一轮输出。
	Step = protocol.Step

	// Response 是一次完整的模型回合。
	Response = protocol.Response
)

// FakeModel implements a scriptable model for tests.
type FakeModel struct {
	mu      sync.Mutex
	steps   []Response
	current int
	// OnNext is called after every call; tests use it to inspect requests/counters.
	OnNext func(req protocol.Request)
}

// New creates a FakeModel from a script of responses.
func New(steps ...Response) *FakeModel {
	return &FakeModel{steps: steps}
}

// Next 实现 orchestrator.DecisionProvider：返回脚本里的下一轮响应。
//
// 参数（ctx 与 request）在这里被忽略——fake 的意义就是不管输入都给出确定输出。
// 需要断言「编排层到底喂了什么给模型」的用例，用 OnNext 取 req。
//
// 脚本耗尽时返回错误，让 ReAct 循环终止而不是空转。
func (f *FakeModel) Next(_ context.Context, req protocol.Request) (Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.OnNext != nil {
		f.OnNext(req)
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

// MustArgsRaw returns the raw JSON for passing directly to a tool.
func MustArgsRaw(raw string) json.RawMessage {
	if !json.Valid([]byte(raw)) {
		panic("fakemodel: 参数不是合法 JSON：" + raw)
	}
	return json.RawMessage(raw)
}
