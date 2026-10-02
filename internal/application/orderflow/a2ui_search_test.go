package orderflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/runevent"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
)

func twoHits() []orderflow.ProductHitLike {
	return []orderflow.ProductHitLike{
		{ProductID: "P1003", Title: "Roamix 防水登山包 30L", Brand: "Roamix",
			Category: "backpack", PriceMajor: 299, Currency: "CNY",
			DefaultSKUID: "P1003-S1", ImageURL: "https://cdn.example/p1003.png"},
		{ProductID: "P1004", Title: "城市通勤包", PriceMajor: 199, Currency: "CNY"},
	}
}

// TestSearchEmitter_三报文且逐条过校验。
//
// 三条报文是有序的：createSurface 建 surface，updateComponents 填组件，
// updateDataModel 绑数据。缺任何一条前端都画不出东西，顺序错了组件会挂到
// 还不存在的 surface 上。
func TestSearchEmitter_三报文且逐条过校验(t *testing.T) {
	t.Parallel()

	msgs, err := orderflow.NewSearchEmitter().Emit(twoHits())
	if err != nil {
		t.Fatalf("Emit 失败：%v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("产出 %d 条报文，期望 3", len(msgs))
	}

	// 用 runevent 自己的校验，而不是复述一遍字段——契约只有一处定义。
	if err := runevent.ValidateCreateSurface(msgs[0]); err != nil {
		t.Errorf("createSurface 不过校验：%v", err)
	}
	if err := runevent.ValidateUpdateComponents(msgs[1]); err != nil {
		t.Errorf("updateComponents 不过校验：%v", err)
	}
	if err := runevent.ValidateUpdateDataModel(msgs[2]); err != nil {
		t.Errorf("updateDataModel 不过校验：%v", err)
	}

	if msgs[0]["action"] != runevent.A2UIActionCreateSurface ||
		msgs[1]["action"] != runevent.A2UIActionUpdateComponents ||
		msgs[2]["action"] != runevent.A2UIActionUpdateDataModel {
		t.Errorf("报文顺序不对：%v / %v / %v", msgs[0]["action"], msgs[1]["action"], msgs[2]["action"])
	}
	if msgs[0]["catalogId"] != runevent.A2UICatalogID {
		t.Errorf("catalogId = %v", msgs[0]["catalogId"])
	}

	// 两张卡，id 稳定为 hit-<index>（与确认单的 card-<i> 分开，避免互相覆盖）。
	comps, _ := msgs[1]["components"].([]any)
	if len(comps) != 2 {
		t.Fatalf("组件数 = %d，期望 2", len(comps))
	}
	for i, raw := range comps {
		c, _ := raw.(map[string]any)
		if want := fmt.Sprintf("hit-%d", i); c["id"] != want {
			t.Errorf("components[%d].id = %v，期望 %s", i, c["id"], want)
		}
		if c["type"] != "ProductCard" {
			t.Errorf("components[%d].type = %v", i, c["type"])
		}
		props, _ := c["props"].(map[string]any)
		// 卡片的键必须是前端认识的那些：sku_id 取默认规格，
		// 少了它前端加购时不知道买哪个规格。
		for _, k := range []string{"product_id", "sku_id", "title", "price_major", "currency"} {
			if _, ok := props[k]; !ok {
				t.Errorf("components[%d] 缺 props.%s：%v", i, k, props)
			}
		}
	}

	first, _ := comps[0].(map[string]any)
	firstProps, _ := first["props"].(map[string]any)
	if firstProps["product_id"] != "P1003" || firstProps["sku_id"] != "P1003-S1" {
		t.Errorf("首卡字段 = %v", firstProps)
	}
	if firstProps["price_major"] != 299.0 {
		t.Errorf("首卡价格 = %v（%T）", firstProps["price_major"], firstProps["price_major"])
	}
}

// TestSearchEmitter_空命中不产出报文。
//
// 一条都没搜到还发一个空 surface，会把上一轮的候选从页面上抹掉——买家看到的是
// 「东西消失了」，而不是「这一轮没找到」。
func TestSearchEmitter_空命中不产出报文(t *testing.T) {
	t.Parallel()

	for _, hits := range [][]orderflow.ProductHitLike{nil, {}} {
		msgs, err := orderflow.NewSearchEmitter().Emit(hits)
		if err != nil {
			t.Errorf("Emit(%v) 报错：%v", hits, err)
		}
		if len(msgs) != 0 {
			t.Errorf("Emit(%v) 产出了 %d 条报文，期望 0", hits, len(msgs))
		}
	}
}

// TestHitsFromToolResult_从工具结果取回命中。
func TestHitsFromToolResult_从工具结果取回命中(t *testing.T) {
	t.Parallel()

	t.Run("正常结果", func(t *testing.T) {
		t.Parallel()
		content := []byte(`{"hits":[{"product_id":"P1003","title":"登山包","price_major":299,"currency":"CNY","default_sku_id":"P1003-S1"}],"recall_strategy":"keyword"}`)
		hits, err := orderflow.HitsFromToolResult(content)
		if err != nil {
			t.Fatalf("解析失败：%v", err)
		}
		if len(hits) != 1 {
			t.Fatalf("命中数 = %d，期望 1", len(hits))
		}
		if hits[0].ProductID != "P1003" || hits[0].DefaultSKUID != "P1003-S1" || hits[0].PriceMajor != 299 {
			t.Errorf("命中 = %+v", hits[0])
		}
	})

	// handler 的空结果把 hits 序列化成 []，而不是省略该键——两种都要能读。
	t.Run("空命中", func(t *testing.T) {
		t.Parallel()
		for _, content := range []string{`{"hits":[]}`, `{"recall_strategy":"keyword"}`} {
			hits, err := orderflow.HitsFromToolResult([]byte(content))
			if err != nil {
				t.Errorf("解析 %s 失败：%v", content, err)
			}
			if len(hits) != 0 {
				t.Errorf("解析 %s 得到 %d 条命中", content, len(hits))
			}
		}
	})

	// 失败态的工具结果 content 是 "error: ..." 的纯文本。
	t.Run("错误态文本", func(t *testing.T) {
		t.Parallel()
		if _, err := orderflow.HitsFromToolResult([]byte("error: 至少需要 normalized_query")); err == nil {
			t.Error("非 JSON 应当报错（调用方据此跳过 A2UI）")
		}
	})
}

// a2uiEventsFrom 从 journal 里挑出 A2UI 事件。
func a2uiEventsFrom(t *testing.T, evs []runevent.Event) []runevent.Event {
	t.Helper()
	var out []runevent.Event
	for _, ev := range evs {
		if ev.Kind == runevent.KindA2UI {
			out = append(out, ev)
		}
	}
	return out
}

// runBridgeWith 用一条 orchestrator 事件驱动 Bridge，返回 journal 里的事件。
func runBridgeWith(t *testing.T, oe orchestrator.Event) []runevent.Event {
	t.Helper()
	b := &orderflow.Bridge{
		Orch:         &fakeOrch{events: []orchestrator.Event{oe}},
		J:            newFakeJournal(),
		DefaultAgent: "main",
	}
	evs, err := b.Run(context.Background(), orderflow.RunnerConfig{
		RunID: "run-a2ui", Query: "找登山包", Agent: "main",
	})
	if err != nil {
		t.Fatalf("Bridge.Run 失败：%v", err)
	}
	return evs
}

// TestBridge_检索结果触发A2UI事件 是端到端那一步：前端等的就是这个 kind。
func TestBridge_检索结果触发A2UI事件(t *testing.T) {
	t.Parallel()

	content := `{"hits":[{"product_id":"P1003","title":"Roamix 防水登山包 30L","price_major":299,"currency":"CNY","default_sku_id":"P1003-S1"}],"recall_strategy":"keyword"}`
	evs := runBridgeWith(t, orchestrator.Event{
		Kind: orchestrator.KindToolResult, Agent: "main",
		ToolName: "product_search_tool", Content: content, Iteration: 0,
	})

	// 三条报文 = 三条事件。前端 A2UIRenderer 对每个载荷跑
	// Zod 的 discriminatedUnion('action')，所以载荷必须**就是**一条报文：
	// 包成 {"messages":[...]} 会被判成「A2UI 报文不合规」并整块渲染失败。
	a2ui := a2uiEventsFrom(t, evs)
	if len(a2ui) != 3 {
		t.Fatalf("A2UI 事件数 = %d，期望 3（一条报文一条；journal 全部事件：%v）",
			len(a2ui), kinds(evs))
	}

	var msgs []map[string]any
	for i, ev := range a2ui {
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			t.Fatalf("第 %d 条 payload 不是合法 JSON：%v（%s）", i, err, ev.Payload)
		}
		// 顶层必须有 action——这正是前端 discriminatedUnion 的判据。
		if _, ok := m["action"].(string); !ok {
			t.Errorf("第 %d 条载荷没有顶层 action，前端会整块拒渲染：%v", i, m)
		}
		msgs = append(msgs, m)
	}

	comps, _ := msgs[1]["components"].([]any)
	if len(comps) != 1 {
		t.Fatalf("组件数 = %d，期望 1", len(comps))
	}
	props, _ := comps[0].(map[string]any)["props"].(map[string]any)
	if props["title"] != "Roamix 防水登山包 30L" {
		t.Errorf("卡片标题 = %v", props["title"])
	}

	// 第一条 A2UI 事件必须紧跟在它那条 tool_result 之后：前端按 seq 顺序消费，
	// 插在别处会让卡片在结果之前渲染出来。三条报文的相对顺序同样由 seq 保证。
	if a2ui[0].RunID != "run-a2ui" {
		t.Errorf("A2UI 事件 run_id = %q", a2ui[0].RunID)
	}
	idx := -1
	for i, ev := range evs {
		if ev.Kind == runevent.KindA2UI {
			idx = i
			break
		}
	}
	if idx <= 0 || evs[idx-1].Kind != runevent.Kind(orchestrator.KindToolResult) {
		t.Errorf("A2UI 事件未紧跟在 tool_result 之后：%v", kinds(evs))
	}
	if evs[idx-1].Seq+1 != a2ui[0].Seq {
		t.Errorf("序号不连续：tool_result seq=%d, a2ui seq=%d", evs[idx-1].Seq, a2ui[0].Seq)
	}
	for i := 1; i < len(a2ui); i++ {
		if a2ui[i].Seq != a2ui[i-1].Seq+1 {
			t.Errorf("第 %d 条 A2UI 报文序号不连续：%d → %d", i, a2ui[i-1].Seq, a2ui[i].Seq)
		}
	}
}

// TestBridge_非检索工具不触发A2UI 断言这个钩子不会对每个工具结果都发一遍。
//
// 载荷故意造得**像**一次检索结果（带 hits 键、字段齐全）：若只喂一个
// `{"confirmation_required":true}`，用例会因为「解析出的 hits 为空」而通过，
// 工具名判断一次都没被执行到——变异测试证实过这种假阳性，所以这里必须
// 让载荷本身具备触发条件，唯一的拦截理由才只剩工具名。
func TestBridge_非检索工具不触发A2UI(t *testing.T) {
	t.Parallel()

	looksLikeHits := `{"hits":[{"product_id":"P1003","title":"混进来的背包","price_major":299,"currency":"CNY","default_sku_id":"P1003-S1"}]}`

	evs := runBridgeWith(t, orchestrator.Event{
		Kind: orchestrator.KindToolResult, Agent: "main",
		ToolName: "create_order_tool",
		Content:  looksLikeHits,
	})
	if got := a2uiEventsFrom(t, evs); len(got) != 0 {
		t.Errorf("非检索工具产出了 %d 条 A2UI 事件", len(got))
	}

	// tool_call 与 model_turn 同样不该触发。
	for _, kind := range []string{orchestrator.KindToolCall, orchestrator.KindModelTurn} {
		evs := runBridgeWith(t, orchestrator.Event{
			Kind: kind, Agent: "main",
			ToolName: "product_search_tool", Content: `{"hits":[{"product_id":"P1"}]}`,
		})
		if got := a2uiEventsFrom(t, evs); len(got) != 0 {
			t.Errorf("%s 事件产出了 %d 条 A2UI 事件", kind, len(got))
		}
	}
}

// TestBridge_检索无命中或失败时不发A2UI。
func TestBridge_检索无命中或失败时不发A2UI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		content string
	}{
		{"空命中", `{"hits":[],"recall_strategy":"keyword"}`},
		{"失败态文本", "error: 账本查询超时"},
		{"content 为空", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			evs := runBridgeWith(t, orchestrator.Event{
				Kind: orchestrator.KindToolResult, Agent: "main",
				ToolName: "product_search_tool", Content: tc.content,
			})
			if got := a2uiEventsFrom(t, evs); len(got) != 0 {
				t.Errorf("产出了 %d 条 A2UI 事件，期望 0", len(got))
			}
		})
	}
}

func kinds(evs []runevent.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, string(ev.Kind))
	}
	return out
}
