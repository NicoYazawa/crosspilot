package orchestrator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/agent/fakemodel"
	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/agent/tools"
)

// TestD1_EndToEnd_PlaceOrder 验收 D1：
//
//	「找一款防水登山包并下单」一轮意图完整跑通并落库。
//
// 落库的真实载体是 create_order_tool 的 handler：测试里用 sync.Map 计数
// 实际落入的订单数据；handler 既执行账本记录（fake）又返回 confirm JSON。
func TestD1_EndToEnd_PlaceOrder(t *testing.T) {
	const orderID = "order-d1-001"

	// --- tool handlers ----------------------------------------------------
	executor := tools.NewExecutor()

	var searchCalled, createCalled, queryCalled int

	executor.Register("product_search_tool", func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
		searchCalled++
		var p struct {
			NormalizedQuery string `json:"normalized_query"`
		}
		_ = json.Unmarshal(args, &p)
		if p.NormalizedQuery == "" {
			return tools.ToolResult{}, errors.New("p.normalized_query 必填")
		}
		body := map[string]any{
			"hits": []map[string]any{
				{"product_id": "P1003", "title": "防水登山包 30L", "price_major": 299.0, "currency": "CNY"},
				{"product_id": "P1004", "title": "城市通勤包", "price_major": 199.0, "currency": "CNY"},
			},
			"recall_strategy": "embedding_rerank",
		}
		b, _ := json.Marshal(body)
		return tools.ToolResult{State: tools.ResultStateSuccess, Content: b}, nil
	})

	executor.Register("create_order_tool", func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
		createCalled++
		var req struct {
			BuyerID string `json:"buyer_id"`
			Items   []struct {
				ProductID string `json:"product_id"`
				SKUID     string `json:"sku_id"`
				Quantity  int    `json:"quantity"`
			} `json:"items"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return tools.ToolResult{State: tools.ResultStateError, Error: err.Error()}, err
		}
		if req.BuyerID == "" || len(req.Items) == 0 {
			return tools.ToolResult{State: tools.ResultStateError, Error: "invalid request"}, errors.New("invalid request")
		}
		// 落库：返回 confirmation_required=true，附 confirmation 单据
		confirmation := map[string]any{
			"confirmation_id": "conf-001",
			"operation_id":    "op-001",
			"buyer_id":        req.BuyerID,
			"session_id":      "session-d1",
			"status":          "pending",
			"expires_at":      "2026-09-30T12:10:00Z",
			"items":           req.Items,
			"subtotal_minor":  29900,
			"currency":        "CNY",
			"snapshot_hash":   "abc123",
		}
		body := map[string]any{
			"confirmation_required": true,
			"confirmation":          confirmation,
		}
		b, _ := json.Marshal(body)
		return tools.ToolResult{State: tools.ResultStateSuccess, Content: b}, nil
	})

	executor.Register("query_order_tool", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		queryCalled++
		body := map[string]any{
			"order_id": orderID,
			"status":   "CONFIRMED",
		}
		b, _ := json.Marshal(body)
		return tools.ToolResult{State: tools.ResultStateSuccess, Content: b}, nil
	})

	// --- fake model script: search → create_order → exit -----------------
	model := fakemodel.New(
		fakemodel.Response{
			Step: fakemodel.Step{
				Content: "我先搜索一下防水登山包",
				Calls: []fakemodel.ToolCall{
					{
						Name:      "product_search_tool",
						Arguments: fakemodel.MustArgsRaw(`{"normalized_query":"防水登山包","top_k":5,"target_currency":"CNY"}`),
					},
				},
			},
		},
		fakemodel.Response{
			Step: fakemodel.Step{
				Content: "找到合适的商品，下单",
				Calls: []fakemodel.ToolCall{
					{
						Name: "create_order_tool",
						Arguments: fakemodel.MustArgsRaw(`{
							"buyer_id":"buyer-d1",
							"session_id":"session-d1",
							"items":[{"product_id":"P1003","sku_id":"P1003-S1","quantity":1}]
						}`),
					},
				},
			},
		},
		fakemodel.Response{
			Step: fakemodel.Step{
				Content: "已下单，等买家确认。",
			},
		},
	)

	o := orchestrator.New(model, executor)

	var events []orchestrator.Event
	res, err := o.Run(context.Background(), orchestrator.Config{
		SessionID:     "session-d1",
		Query:         "找一款防水登山包并下单",
		Agent:         "main",
		MaxIterations: 8,
		OnEvent: func(ev orchestrator.Event) {
			events = append(events, ev)
		},
	})
	if err != nil {
		t.Fatalf("D1 Run 失败：%v", err)
	}

	// --- D1 验收断言 -------------------------------------------------------
	if searchCalled != 1 {
		t.Errorf("产品搜索应被调用 1 次，实际 %d 次", searchCalled)
	}
	if createCalled != 1 {
		t.Errorf("下单应被调用 1 次，实际 %d 次", createCalled)
	}
	if model.CallsSoFar() != 3 {
		t.Errorf("模型应被调用 3 次（search + create + exit），实际 %d 次", model.CallsSoFar())
	}
	if len(res.Transcript) != 3 {
		t.Fatalf("应有 3 个 turn，实际 %d 个", len(res.Transcript))
	}
	// 第三轮的 turn 不应有工具调用
	if len(res.Transcript[2].ToolCalls) != 0 {
		t.Errorf("最后一轮（exit）不应有工具调用，实际 %d", len(res.Transcript[2].ToolCalls))
	}

	// 流事件必须包含 run_start / tool_call / run_finished
	hasStart, hasFinish := false, false
	for _, ev := range events {
		if ev.Kind == orchestrator.KindRunStart {
			hasStart = true
		}
		if ev.Kind == orchestrator.KindRunFinished {
			hasFinish = true
		}
	}
	if !hasStart {
		t.Error("事件流缺少 run_start")
	}
	if !hasFinish {
		t.Error("事件流缺少 run_finished")
	}
}

// TestD1_MissingTool_GracefullyHandled 验证：当模型尝试调用未注册工具时，
// orchestrator 不崩溃，把错误回报给模型继续运行。
func TestD1_MissingTool_GracefullyHandled(t *testing.T) {
	executor := tools.NewExecutor()
	executor.Register("product_search_tool", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		b, _ := json.Marshal(map[string]any{"hits": []any{}, "recall_strategy": "keyword_2gram"})
		return tools.ToolResult{State: tools.ResultStateSuccess, Content: b}, nil
	})

	model := fakemodel.New(
		fakemodel.Response{
			Step: fakemodel.Step{
				Calls: []fakemodel.ToolCall{
					{Name: "product_search_tool", Arguments: fakemodel.MustArgsRaw(`{"normalized_query":"x"}`)},
					// 故意打错名字，确认 orchestrator 不 panic
					{Name: "unknown_tool_xyz", Arguments: fakemodel.MustArgsRaw(`{}`)},
				},
			},
		},
		fakemodel.Response{
			Step: fakemodel.Step{Content: "完成"},
		},
	)

	o := orchestrator.New(model, executor)
	res, err := o.Run(context.Background(), orchestrator.Config{
		SessionID: "s1", Query: "测试", Agent: "main",
		MaxIterations: 4,
	})
	if err != nil {
		t.Fatalf("D1 missing-tool 不应 panic，got %v", err)
	}
	if len(res.Transcript) != 2 {
		t.Fatalf("期望 2 turn，got %d", len(res.Transcript))
	}
}

// TestD2_TaskDispatch_Isolation 验收 D2：task_dispatch 并发派发 3 个子 Agent
// 各自维护独立的上下文，互不污染。
func TestD2_TaskDispatch_Isolation(t *testing.T) {
	executor := tools.NewExecutor()

	var calls []string
	executor.Register("task_dispatch_tool", func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
		var req struct {
			Agent  string `json:"agent"`
			Prompt string `json:"prompt"`
		}
		_ = json.Unmarshal(args, &req)
		calls = append(calls, req.Agent+":"+req.Prompt)
		body, _ := json.Marshal(map[string]any{"agent": req.Agent, "status": "dispatched"})
		return tools.ToolResult{State: tools.ResultStateSuccess, Content: body}, nil
	})

	// 模型一次性发起 3 个并发派发
	model := fakemodel.New(
		fakemodel.Response{
			Step: fakemodel.Step{
				Calls: []fakemodel.ToolCall{
					{Name: "task_dispatch_tool", Arguments: fakemodel.MustArgsRaw(`{"agent":"search","prompt":"查找登山包"}`)},
					{Name: "task_dispatch_tool", Arguments: fakemodel.MustArgsRaw(`{"agent":"trade","prompt":"下单"}`)},
					{Name: "task_dispatch_tool", Arguments: fakemodel.MustArgsRaw(`{"agent":"memory","prompt":"写入"}`)},
				},
			},
		},
		fakemodel.Response{
			Step: fakemodel.Step{Content: "三个派发已完成"},
		},
	)

	o := orchestrator.New(model, executor)
	_, err := o.Run(context.Background(), orchestrator.Config{
		SessionID: "s2", Query: "派发", Agent: "main",
		MaxIterations: 4,
	})
	if err != nil {
		t.Fatalf("D2 Run 失败：%v", err)
	}

	if len(calls) != 3 {
		t.Fatalf("期望 3 次派发，实际 %d", len(calls))
	}
	// 各自 agent 独立，未串味
	expected := []string{"search:查找登山包", "trade:下单", "memory:写入"}
	for i, want := range expected {
		if calls[i] != want {
			t.Errorf("第 %d 次派发污染：got %q want %q", i+1, calls[i], want)
		}
	}
}

// TestOrchestrator_summary_Events 验证 summary 对不同 tool result 产生正确的事件内容。
// 这间接覆盖了 summary 函数的三个分支：error / empty content / normal content。
func TestOrchestrator_summary_Events(t *testing.T) {
	executor := tools.NewExecutor()

	// 注册一个返回空内容的工具
	executor.Register("echo_tool", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{State: tools.ResultStateSuccess, Content: nil}, nil
	})

	// 注册一个返回错误状态的工具
	executor.Register("fail_tool", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{State: tools.ResultStateError, Error: "something went wrong"}, nil
	})

	// 注册一个正常返回内容的工具
	executor.Register("ok_tool", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		b, _ := json.Marshal(map[string]any{"result": "ok"})
		return tools.ToolResult{State: tools.ResultStateSuccess, Content: b}, nil
	})

	model := fakemodel.New(
		fakemodel.Response{
			Step: fakemodel.Step{
				Content: "调用三个工具",
				Calls: []fakemodel.ToolCall{
					{Name: "echo_tool", Arguments: fakemodel.MustArgsRaw(`{}`)},
					{Name: "fail_tool", Arguments: fakemodel.MustArgsRaw(`{}`)},
					{Name: "ok_tool", Arguments: fakemodel.MustArgsRaw(`{}`)},
				},
			},
		},
		fakemodel.Response{
			Step: fakemodel.Step{Content: "完成"},
		},
	)

	o := orchestrator.New(model, executor)

	var events []orchestrator.Event
	_, _ = o.Run(context.Background(), orchestrator.Config{
		SessionID:     "s-summary",
		Query:         "测试 summary",
		Agent:         "main",
		MaxIterations: 4,
		OnEvent: func(ev orchestrator.Event) {
			events = append(events, ev)
		},
	})

	// 找 tool_result 事件，验证三种分支
	var echoContent, failContent, okContent string
	for i := 0; i < len(events); i++ {
		ev := events[i]
		if ev.Kind == orchestrator.KindToolResult {
			switch ev.ToolName {
			case "echo_tool":
				echoContent = ev.Content
			case "fail_tool":
				failContent = ev.Content
			case "ok_tool":
				okContent = ev.Content
			}
		}
	}

	// empty content → 空字符串
	if echoContent != "" {
		t.Errorf("echo_tool (empty) 应返回空字符串，实际 %q", echoContent)
	}
	// error state → "error: " + error message
	if failContent != "error: something went wrong" {
		t.Errorf("fail_tool (error) 应返回 'error: something went wrong'，实际 %q", failContent)
	}
	// normal content → Content 字段
	if okContent == "" {
		t.Error("ok_tool 应返回正常内容")
	}
}

// TestOrchestrator_多轮往返时history正确累加 是接入真实模型后最关键的一条不变量。
//
// 模型没有记忆：它每一轮看到的全部上下文就是这次请求里的 history。少一条
// assistant 消息，模型就不知道自己刚才发起过调用；tool_call_id 对不上，上游
// 会直接以 400 拒绝整个请求。两条都不是「效果差一点」，而是链路根本走不通。
func TestOrchestrator_多轮往返时history正确累加(t *testing.T) {
	executor := tools.NewExecutor()
	executor.Register("product_search_tool", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{
			State:   tools.ResultStateSuccess,
			Content: json.RawMessage(`{"hits":[{"product_id":"P1003"}]}`),
		}, nil
	})
	executor.Register("fail_tool", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{State: tools.ResultStateError, Error: "库存服务不可用"}, nil
	})
	executor.Register("ok_tool", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{State: tools.ResultStateSuccess, Content: json.RawMessage(`{"stock":3}`)}, nil
	})

	model := fakemodel.New(
		fakemodel.Response{Step: fakemodel.Step{
			Content: "先搜一下",
			Calls: []fakemodel.ToolCall{{
				ID: "call_1", Name: "product_search_tool",
				Arguments: fakemodel.MustArgsRaw(`{"normalized_query":"登山包"}`),
			}},
		}},
		fakemodel.Response{Step: fakemodel.Step{
			Content: "再查库存",
			Calls: []fakemodel.ToolCall{
				// 故意不给 ID：tool 消息的 tool_call_id 是必填的，
				// 缺了它模型分不清结果属于哪次调用。
				{Name: "fail_tool", Arguments: fakemodel.MustArgsRaw(`{}`)},
				{Name: "ok_tool", Arguments: fakemodel.MustArgsRaw(`{}`)},
			},
		}},
		fakemodel.Response{Step: fakemodel.Step{Content: "完成"}},
	)

	var requests []protocol.Request
	model.OnNext = func(req protocol.Request) { requests = append(requests, req) }

	o := orchestrator.New(model, executor)
	o.SystemPrompt = "你是购物助手"
	o.Tools = []protocol.ToolDef{{Name: "product_search_tool"}}

	if _, err := o.Run(context.Background(), orchestrator.Config{
		Query: "找登山包", Agent: "main", MaxIterations: 8,
	}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	if len(requests) != 3 {
		t.Fatalf("模型被调用 %d 次，期望 3 次", len(requests))
	}

	// 每一轮都必须带上系统提示词与工具声明——漏掉任一轮，模型那一轮就成了瞎子。
	for i, req := range requests {
		if req.System != "你是购物助手" {
			t.Errorf("第 %d 轮丢了系统提示词：%q", i, req.System)
		}
		if len(req.Tools) != 1 {
			t.Errorf("第 %d 轮丢了工具声明：%d 个", i, len(req.Tools))
		}
	}

	// 第一轮：只有买家那句话。
	if got := len(requests[0].History); got != 1 {
		t.Fatalf("首轮 history 长度 = %d，期望 1", got)
	}
	if m := requests[0].History[0]; m.Role != protocol.RoleUser || m.Content != "找登山包" {
		t.Errorf("首条消息 = %+v", m)
	}

	// 第三轮才是完整形态：user + 两轮 (assistant + tool)。
	h := requests[2].History
	if len(h) != 6 {
		t.Fatalf("第三轮 history 长度 = %d，期望 6：%+v", len(h), h)
	}

	// [1] assistant 必须带上自己发起的调用，否则模型不知道结果是什么的回执。
	if h[1].Role != protocol.RoleAssistant || h[1].Content != "先搜一下" {
		t.Errorf("history[1] = %+v", h[1])
	}
	if len(h[1].ToolCalls) != 1 || h[1].ToolCalls[0].ID != "call_1" ||
		h[1].ToolCalls[0].Name != "product_search_tool" {
		t.Errorf("history[1].ToolCalls = %+v", h[1].ToolCalls)
	}
	if string(h[1].ToolCalls[0].Arguments) != `{"normalized_query":"登山包"}` {
		t.Errorf("调用参数被改写：%s", h[1].ToolCalls[0].Arguments)
	}

	// [2] tool 消息带上游给的 ID，内容就是 handler 的原始 JSON。
	if h[2].Role != protocol.RoleTool || h[2].ToolCallID != "call_1" {
		t.Errorf("history[2] = %+v", h[2])
	}
	if h[2].Content != `{"hits":[{"product_id":"P1003"}]}` {
		t.Errorf("工具结果被改写：%q", h[2].Content)
	}

	// [4][5] 模型没给 ID 时合成的 ID 必须与 assistant 里那条严格一致；
	// 而且要按调用序号区分同一轮里的多个工具。
	if h[3].Role != protocol.RoleAssistant {
		t.Fatalf("history[3] = %+v，期望 assistant", h[3])
	}
	if got := []string{h[3].ToolCalls[0].ID, h[3].ToolCalls[1].ID}; got[0] != "call_1_0" || got[1] != "call_1_1" {
		t.Errorf("合成的调用 ID = %v，期望 [call_1_0 call_1_1]", got)
	}
	for i, tc := range h[3].ToolCalls {
		if h[4+i].ToolCallID != tc.ID {
			t.Errorf("第 %d 个 tool 消息的 tool_call_id = %q，与 assistant 里的 %q 不匹配",
				i, h[4+i].ToolCallID, tc.ID)
		}
	}

	// 失败的工具结果要以 "error: " 前缀回给模型，它才知道这次调用没成功。
	if h[4].Content != "error: 库存服务不可用" {
		t.Errorf("失败结果未标注：%q", h[4].Content)
	}
	if h[5].Content != `{"stock":3}` {
		t.Errorf("成功结果被改写：%q", h[5].Content)
	}
}

// TestOrchestrator_Run_EmptyEvents 验证无工具调用时的 summary 行为。
func TestOrchestrator_Run_EmptyEvents(t *testing.T) {
	executor := tools.NewExecutor()

	model := fakemodel.New(
		fakemodel.Response{
			Step: fakemodel.Step{Content: "直接结束，无需工具"},
		},
	)

	o := orchestrator.New(model, executor)

	var events []orchestrator.Event
	res, err := o.Run(context.Background(), orchestrator.Config{
		SessionID:     "s-empty",
		Query:         "直接结束",
		Agent:         "main",
		MaxIterations: 4,
		OnEvent: func(ev orchestrator.Event) {
			events = append(events, ev)
		},
	})

	if err != nil {
		t.Fatalf("Run 不应报错，got %v", err)
	}
	if len(res.Transcript) != 1 {
		t.Fatalf("应有 1 个 turn，实际 %d", len(res.Transcript))
	}
	if len(res.Transcript[0].ToolCalls) != 0 {
		t.Error("不应有工具调用")
	}
}
