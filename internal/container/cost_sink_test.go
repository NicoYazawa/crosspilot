package container

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/agent/orchestrator"
	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/application/orderflow"
	domainobs "github.com/NicoYazawa/crosspilot/internal/domain/observability"
	"github.com/NicoYazawa/crosspilot/internal/pricing"
)

// captureSink 是下游 Sink 的替身：留档收到的批，不碰数据库。
type captureSink struct {
	got    []domainobs.SinkRecord
	closed bool
}

func (s *captureSink) Append(_ context.Context, batch []domainobs.SinkRecord) error {
	s.got = append(s.got, batch...)
	return nil
}

func (s *captureSink) Close() error { s.closed = true; return nil }

// testBook 是与内嵌表同形的最小价格表。
func testBook(t *testing.T) *pricing.PriceBook {
	t.Helper()
	book, err := pricing.LoadFromReader(strings.NewReader(`
version: "test"
entries:
  - {provider: deepseek, model: deepseek-flash, kind: input,        unit_minor: "2",    currency: CNY}
  - {provider: deepseek, model: deepseek-flash, kind: output,       unit_minor: "8",    currency: CNY}
  - {provider: deepseek, model: deepseek-flash, kind: cached_input, unit_minor: "0.04", currency: CNY}
`), "test://cost-sink")
	if err != nil {
		t.Fatalf("构造测试价格表失败: %v", err)
	}
	return book
}

// modelTurnRecord 造一条已脱敏的 model_turn 记录。
func modelTurnRecord(t *testing.T, payload orderflow.ModelTurnPayload) domainobs.SinkRecord {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("payload 序列化失败: %v", err)
	}
	return domainobs.SinkRecord{
		EventID: "evt-1", RunID: "run-1", Seq: 1,
		Kind: orchestrator.KindModelTurn, Agent: "main",
		PayloadRedacted: raw, CreatedAt: 1_700_000_000,
	}
}

func newTestSink(t *testing.T, book *pricing.PriceBook, id modelIdentity) (*pricingSink, *captureSink) {
	t.Helper()
	inner := &captureSink{}
	s := newPricingSink(inner, book, id, slog.New(slog.DiscardHandler))
	ps, ok := s.(*pricingSink)
	if !ok {
		t.Fatalf("期望 *pricingSink，实际 %T", s)
	}
	return ps, inner
}

var testIdentity = modelIdentity{Provider: "deepseek", Model: "deepseek-flash"}

// TestPricingSink_已定价 断言正常路径：用量解密 → 查表 → 费用挂到记录上。
func TestPricingSink_已定价(t *testing.T) {
	t.Parallel()
	ps, inner := newTestSink(t, testBook(t), testIdentity)

	rec := modelTurnRecord(t, orderflow.ModelTurnPayload{
		Agent: "main", Content: "好的", Iteration: 0,
		Provider: "deepseek", Model: "deepseek-flash",
		Usage: &protocol.Usage{InputTokens: 3600, OutputTokens: 800, CachedTokens: 200, ReasoningTokens: 64},
	})
	if err := ps.Append(context.Background(), []domainobs.SinkRecord{rec}); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}

	if len(inner.got) != 1 {
		t.Fatalf("下游收到 %d 条，期望 1 条", len(inner.got))
	}
	c := inner.got[0].Cost
	if c == nil {
		t.Fatal("Cost 为 nil，成本没被挂上")
	}
	// 全价 input 3400 × 2 + output 800 × 8 + cached 200 × 0.04 = 6800 + 6400 + 8
	if c.CostMinor != 13208 {
		t.Errorf("CostMinor = %d, 期望 13208", c.CostMinor)
	}
	if c.Unpriced {
		t.Error("不该标 unpriced")
	}
	if c.Currency != "CNY" {
		t.Errorf("Currency = %q, 期望 CNY", c.Currency)
	}
	// reasoning 只留痕不参与计费：它的值要如实落库
	if c.TokensReasoning != 64 {
		t.Errorf("TokensReasoning = %d, 期望 64（落库留痕）", c.TokensReasoning)
	}
	if c.Provider != "deepseek" || c.Model != "deepseek-flash" {
		t.Errorf("身份 = %s/%s", c.Provider, c.Model)
	}
}

// TestPricingSink_非模型事件不挂成本 断言工具调用、A2UI 报文不产生成本行。
//
// 若给它们各写一行全 0 的成本，「不涉及计费」与「花了 0 元」在表里会长得一样，
// 成本看板的 total_calls 也会虚高，F3 的对账更无从谈起。
func TestPricingSink_非模型事件不挂成本(t *testing.T) {
	t.Parallel()
	ps, inner := newTestSink(t, testBook(t), testIdentity)

	batch := []domainobs.SinkRecord{
		{EventID: "e1", RunID: "run-1", Kind: orchestrator.KindToolCall, PayloadRedacted: []byte(`{}`)},
		{EventID: "e2", RunID: "run-1", Kind: orchestrator.KindToolResult, PayloadRedacted: []byte(`{}`)},
		{EventID: "e3", RunID: "run-1", Kind: "a2ui", PayloadRedacted: []byte(`{}`)},
		{EventID: "e4", RunID: "run-1", Kind: orchestrator.KindRunStart, PayloadRedacted: []byte(`{}`)},
	}
	if err := ps.Append(context.Background(), batch); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}
	for i, rec := range inner.got {
		if rec.Cost != nil {
			t.Errorf("第 %d 条（%s）不该有成本行", i, rec.Kind)
		}
	}
}

// TestPricingSink_无usage标unpriced 断言「不知道花了多少」不会被记成「没花钱」。
//
// 上游不返回 usage 是常见情况（不少兼容网关如此）。这条调用确实产生了 token，
// 只是我们没拿到数字——按 0 元落库会让成本看板系统性低报。
func TestPricingSink_无usage标unpriced(t *testing.T) {
	t.Parallel()
	ps, inner := newTestSink(t, testBook(t), testIdentity)

	rec := modelTurnRecord(t, orderflow.ModelTurnPayload{
		Agent: "main", Content: "好的",
		Provider: "deepseek", Model: "deepseek-flash",
		Usage: nil,
	})
	if err := ps.Append(context.Background(), []domainobs.SinkRecord{rec}); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}

	c := inner.got[0].Cost
	if c == nil {
		t.Fatal("无 usage 也必须落一条成本行，否则这次调用在成本表里彻底消失")
	}
	if !c.Unpriced {
		t.Error("无 usage 必须标 unpriced")
	}
	if c.CostMinor != 0 {
		t.Errorf("CostMinor = %d, 期望 0", c.CostMinor)
	}
}

// TestPricingSink_未知model标unpriced 断言价格表没覆盖时显式标记。
func TestPricingSink_未知model标unpriced(t *testing.T) {
	t.Parallel()
	ps, inner := newTestSink(t, testBook(t), testIdentity)

	rec := modelTurnRecord(t, orderflow.ModelTurnPayload{
		Agent: "main", Provider: "deepseek", Model: "deepseek-chat",
		Usage: &protocol.Usage{InputTokens: 100, OutputTokens: 50},
	})
	if err := ps.Append(context.Background(), []domainobs.SinkRecord{rec}); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}

	c := inner.got[0].Cost
	if c == nil || !c.Unpriced {
		t.Fatalf("未覆盖的 model 必须标 unpriced，实际 %+v", c)
	}
	if c.CostMinor != 0 {
		t.Errorf("unpriced 时 cost_minor 应为 0，实际 %d", c.CostMinor)
	}
}

// TestPricingSink_payload解析失败仍落unpriced 断言坏 payload 不让成本行消失。
//
// 返回 nil 会让这次调用在成本表里没有任何痕迹——比「有一条标着不知道多少钱的
// 记录」糟得多，因为前者连「这里有一次调用」都看不到。
func TestPricingSink_payload解析失败仍落unpriced(t *testing.T) {
	t.Parallel()
	ps, inner := newTestSink(t, testBook(t), testIdentity)

	rec := domainobs.SinkRecord{
		EventID: "e-bad", RunID: "run-1", Kind: orchestrator.KindModelTurn,
		PayloadRedacted: []byte(`{不是合法 json`),
	}
	if err := ps.Append(context.Background(), []domainobs.SinkRecord{rec}); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}

	c := inner.got[0].Cost
	if c == nil {
		t.Fatal("坏 payload 也必须落一条成本行")
	}
	if !c.Unpriced {
		t.Error("解析不出来 = 不知道花了多少，必须标 unpriced")
	}
	// 身份退回到配置声明的值，便于排障时定位是哪一家
	if c.Provider != "deepseek" || c.Model != "deepseek-flash" {
		t.Errorf("应退回配置身份，实际 %s/%s", c.Provider, c.Model)
	}
}

// TestPricingSink_payload身份优先于配置 断言故障转移时成本归因不会错位。
//
// 配置里写了 A 家，但这一轮实际由 B 家回答（failover / A-B）。若按配置的常量
// 定价，B 家的用量会被算到 A 家的价目上——两个数字都错，且从表里看不出错。
func TestPricingSink_payload身份优先于配置(t *testing.T) {
	t.Parallel()
	// 配置声明的是 deepseek-flash，但 payload 说这次是别的 model
	ps, inner := newTestSink(t, testBook(t), testIdentity)

	rec := modelTurnRecord(t, orderflow.ModelTurnPayload{
		Agent: "main", Provider: "deepseek", Model: "deepseek-chat",
		Usage: &protocol.Usage{InputTokens: 100},
	})
	if err := ps.Append(context.Background(), []domainobs.SinkRecord{rec}); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}

	c := inner.got[0].Cost
	if c.Model != "deepseek-chat" {
		t.Errorf("Model = %q, 期望取 payload 的 deepseek-chat 而不是配置值", c.Model)
	}
	if !c.Unpriced {
		t.Error("deepseek-chat 不在表里，应标 unpriced")
	}
}

// TestPricingSink_payload缺身份时退回配置 断言字段缺失时的兜底。
func TestPricingSink_payload缺身份时退回配置(t *testing.T) {
	t.Parallel()
	ps, inner := newTestSink(t, testBook(t), testIdentity)

	rec := modelTurnRecord(t, orderflow.ModelTurnPayload{
		Agent: "main", Content: "好的",
		Usage: &protocol.Usage{InputTokens: 100, OutputTokens: 0},
	})
	if err := ps.Append(context.Background(), []domainobs.SinkRecord{rec}); err != nil {
		t.Fatalf("Append 失败: %v", err)
	}

	c := inner.got[0].Cost
	if c.Provider != "deepseek" || c.Model != "deepseek-flash" {
		t.Errorf("应退回配置身份，实际 %s/%s", c.Provider, c.Model)
	}
	if c.Unpriced {
		t.Error("身份可由配置补齐，不该 unpriced")
	}
	// 100 × 2 = 200
	if c.CostMinor != 200 {
		t.Errorf("CostMinor = %d, 期望 200", c.CostMinor)
	}
}

// TestNewPricingSink_空表直接透传 断言没有价格表时不做无谓的包装。
//
// 空表下每一条都会是 unpriced，包装只会让每条都白跑一次查表 + decimal 运算，
// 而结果完全相同。
func TestNewPricingSink_空表直接透传(t *testing.T) {
	t.Parallel()
	inner := &captureSink{}
	logger := slog.New(slog.DiscardHandler)

	empty, err := pricing.LoadFromReader(strings.NewReader(`version: "t"`), "test://empty")
	if err != nil {
		t.Fatalf("构造空表失败: %v", err)
	}

	for _, book := range []*pricing.PriceBook{empty, nil} {
		got := newPricingSink(inner, book, testIdentity, logger)
		if _, wrapped := got.(*pricingSink); wrapped {
			t.Errorf("空表应原样返回 inner，实际包了一层 %T", got)
		}
		if got != inner {
			t.Errorf("空表应原样返回 inner，实际 %T", got)
		}
	}
}

// TestPricingSink_Close透传 断言装饰器不吞掉下游的关闭。
func TestPricingSink_Close透传(t *testing.T) {
	t.Parallel()
	ps, inner := newTestSink(t, testBook(t), testIdentity)

	if err := ps.Close(); err != nil {
		t.Fatalf("Close 失败: %v", err)
	}
	if !inner.closed {
		t.Error("Close 没有透传到下游 Sink——连接池的收尾会被漏掉")
	}
}
