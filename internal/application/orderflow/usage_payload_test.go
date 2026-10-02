package orderflow_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/observability"
	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
)

// TestForward_用量与身份透传到观测副本 是本包最要紧的一条断言。
//
// 成本归因的读侧（装配根的成本装饰器）只认 payload 里的 usage / provider /
// model：这一层如果把它们丢了，后面价格表再准也只会得到全量 unpriced。
// 此前 payload 是就地手拼的白名单 map，模型返回什么都不会透传——用量在
// 这里静默消失过一次，所以用一条端到端的断言把它钉住。
func TestForward_用量与身份透传到观测副本(t *testing.T) {
	t.Parallel()

	o := &fakeOrch{events: []orchestrator.Event{
		{Kind: orchestrator.KindModelTurn, Agent: "main", Content: "我来找", Iteration: 0,
			Provider: "deepseek", Model: "deepseek-flash",
			Usage: &protocol.Usage{InputTokens: 3600, OutputTokens: 800, CachedTokens: 200, ReasoningTokens: 64}},
		{Kind: orchestrator.KindToolCall, Agent: "main", ToolName: "product_search_tool", Iteration: 0,
			Content: `{"query":"登山包"}`},
	}}
	j := newFakeJournal()
	sink := &recSink{}
	emitter := observability.NewEmitter(sink, observability.EmitterConfig{
		QueueSize:  64,
		BatchSize:  1,
		FlushEvery: 20 * time.Millisecond,
		Redactor:   noopRedactor{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	emitter.Start(ctx)

	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main", Emitter: emitter}
	if _, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r-usage", SessionID: "s", Query: "登山包",
	}); err != nil {
		t.Fatalf("Run 失败：%v", err)
	}
	if err := emitter.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}

	recs := sink.Records()
	modelTurn, toolCall, okTurn, okTool := observability.SinkRecord{}, observability.SinkRecord{}, false, false
	for _, rec := range recs {
		switch rec.Kind {
		case orchestrator.KindModelTurn:
			modelTurn, okTurn = rec, true
		case orchestrator.KindToolCall:
			toolCall, okTool = rec, true
		}
	}
	if !okTurn || !okTool {
		t.Fatalf("观测副本缺少事件：model_turn=%v tool_call=%v", okTurn, okTool)
	}

	var p orderflow.ModelTurnPayload
	if err := json.Unmarshal(modelTurn.PayloadRedacted, &p); err != nil {
		t.Fatalf("payload 解析失败：%v（%s）", err, modelTurn.PayloadRedacted)
	}
	if p.Usage == nil {
		t.Fatal("model_turn 的 payload 里没有 usage——用量在这一层丢了")
	}
	if p.Usage.InputTokens != 3600 || p.Usage.OutputTokens != 800 ||
		p.Usage.CachedTokens != 200 || p.Usage.ReasoningTokens != 64 {
		t.Errorf("usage 值不对：%+v", *p.Usage)
	}
	if p.Provider != "deepseek" || p.Model != "deepseek-flash" {
		t.Errorf("身份丢失：provider=%q model=%q", p.Provider, p.Model)
	}

	// 工具事件不该带这三个键：给每次工具调用都塞一份 usage/provider/model
	// 会让读侧分不清「哪条事件真的产生过费用」。
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(toolCall.PayloadRedacted, &raw); err != nil {
		t.Fatalf("工具事件 payload 解析失败：%v", err)
	}
	for _, key := range []string{"usage", "provider", "model"} {
		if _, exists := raw[key]; exists {
			t.Errorf("工具事件不该带 %q 键", key)
		}
	}
	// 而不带 omitempty 的四个既有字段必须仍在：它们的线上字节不能变。
	for _, key := range []string{"agent", "content", "tool_name", "iteration"} {
		if _, exists := raw[key]; !exists {
			t.Errorf("工具事件缺少既有键 %q", key)
		}
	}
}

// TestForward_无用量时不写usage键 断言「上游没返回用量」与「用量为 0」在
// payload 里也分得开：前者不写键（Usage 为 nil），后者写一组 0。
func TestForward_无用量时不写usage键(t *testing.T) {
	t.Parallel()

	o := &fakeOrch{events: []orchestrator.Event{
		{Kind: orchestrator.KindModelTurn, Agent: "main", Content: "好的", Iteration: 0},
	}}
	j := newFakeJournal()
	b := &orderflow.Bridge{Orch: o, J: j, DefaultAgent: "main"}

	evs, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "r-nousage", SessionID: "s", Query: "q",
	})
	if err != nil {
		t.Fatalf("Run 失败：%v", err)
	}

	var found bool
	for _, ev := range evs {
		if string(ev.Kind) != orchestrator.KindModelTurn {
			continue
		}
		found = true
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(ev.Payload, &raw); err != nil {
			t.Fatalf("payload 解析失败：%v", err)
		}
		if _, exists := raw["usage"]; exists {
			t.Error("上游没返回用量时不该写 usage 键")
		}
	}
	if !found {
		t.Fatal("没找到 model_turn 事件")
	}
}
