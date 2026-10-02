package orchestrator_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/agent/fakemodel"
	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/agent/tools"
)

// TestRun_modelTurn事件携带用量与身份 断言编排层只做转手、不丢字段。
//
// 编排层不解释用量，但它是模型适配器与下游（journal / 观测副本 / 成本归因）
// 之间唯一的那道门。这道门漏一个字段，上面采得再准也到不了价格表。
func TestRun_modelTurn事件携带用量与身份(t *testing.T) {
	model := fakemodel.New(
		fakemodel.Response{Step: fakemodel.Step{
			Content:  "我来查一下",
			Provider: "deepseek",
			Model:    "deepseek-flash",
			Usage: &protocol.Usage{
				InputTokens: 3600, OutputTokens: 800, CachedTokens: 200, ReasoningTokens: 64,
			},
		}},
	)
	o := orchestrator.New(model, tools.NewExecutor())

	var turns []orchestrator.Event
	_, err := o.Run(context.Background(), orchestrator.Config{
		SessionID: "s-usage", Query: "登山包", Agent: "main",
		OnEvent: func(ev orchestrator.Event) {
			if ev.Kind == orchestrator.KindModelTurn {
				turns = append(turns, ev)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("model_turn 事件数 = %d，期望 1", len(turns))
	}

	ev := turns[0]
	if ev.Usage == nil {
		t.Fatal("model_turn 没带 Usage——用量在编排层丢了")
	}
	if ev.Usage.InputTokens != 3600 || ev.Usage.OutputTokens != 800 ||
		ev.Usage.CachedTokens != 200 || ev.Usage.ReasoningTokens != 64 {
		t.Errorf("Usage = %+v", *ev.Usage)
	}
	if ev.Provider != "deepseek" || ev.Model != "deepseek-flash" {
		t.Errorf("身份丢失：provider=%q model=%q", ev.Provider, ev.Model)
	}
}

// TestRun_上游无用量时Usage为nil 断言编排层不替上游编造用量。
//
// 补一个零值 Usage 会让下游看到「用量确实是 0」，于是把一次不知道花费的调用
// 记成 0 元——正是 F4 要防的那件事。nil 才如实表达「不知道」。
func TestRun_上游无用量时Usage为nil(t *testing.T) {
	model := fakemodel.New(fakemodel.Response{Step: fakemodel.Step{Content: "好的"}})
	o := orchestrator.New(model, tools.NewExecutor())

	var found bool
	_, err := o.Run(context.Background(), orchestrator.Config{
		SessionID: "s-nousage", Query: "q", Agent: "main",
		OnEvent: func(ev orchestrator.Event) {
			if ev.Kind != orchestrator.KindModelTurn {
				return
			}
			found = true
			if ev.Usage != nil {
				t.Errorf("上游没给用量时不该编一个：%+v", *ev.Usage)
			}
			if ev.Provider != "" || ev.Model != "" {
				t.Errorf("上游没给身份时不该编：%q/%q", ev.Provider, ev.Model)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if !found {
		t.Fatal("没发出 model_turn 事件")
	}
}

// TestRun_工具事件不带用量 断言用量只挂在它真正来自的那一轮上。
//
// 若工具事件也带一份用量，读侧会把「每次工具调用」也当成一次计费调用，
// 成本看板的调用次数与实际模型调用轮数对不上。
func TestRun_工具事件不带用量(t *testing.T) {
	executor := tools.NewExecutor()
	executor.Register("echo_tool", func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
		return tools.ToolResult{State: tools.ResultStateSuccess, Content: []byte(`{"ok":true}`)}, nil
	})

	model := fakemodel.New(
		fakemodel.Response{Step: fakemodel.Step{
			Content:  "调用工具",
			Provider: "deepseek", Model: "deepseek-flash",
			Usage: &protocol.Usage{InputTokens: 100, OutputTokens: 10},
			Calls: []fakemodel.ToolCall{{Name: "echo_tool", Arguments: fakemodel.MustArgsRaw(`{}`)}},
		}},
		fakemodel.Response{Step: fakemodel.Step{Content: "完成"}},
	)
	o := orchestrator.New(model, executor)

	var toolEvents int
	_, err := o.Run(context.Background(), orchestrator.Config{
		SessionID: "s-tools", Query: "q", Agent: "main", MaxIterations: 4,
		OnEvent: func(ev orchestrator.Event) {
			if ev.Kind != orchestrator.KindToolCall && ev.Kind != orchestrator.KindToolResult {
				return
			}
			toolEvents++
			if ev.Usage != nil {
				t.Errorf("%s 事件不该带 usage：%+v", ev.Kind, *ev.Usage)
			}
		},
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if toolEvents != 2 {
		t.Errorf("工具事件数 = %d，期望 2（一次调用一次结果）", toolEvents)
	}
}
