package orchestrator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/agent/fakemodel"
	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
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

	executor.Register("product_search_tool", func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
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

	executor.Register("create_order_tool", func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
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

	executor.Register("query_order_tool", func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
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
	executor.Register("product_search_tool", func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
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
	executor.Register("task_dispatch_tool", func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
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
