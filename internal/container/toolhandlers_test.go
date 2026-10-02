package container

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/agent/llm"
	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
	"github.com/NicoYazawa/crosspilot/internal/agent/tools"
	"github.com/NicoYazawa/crosspilot/internal/application/catalogsearch"
	"github.com/NicoYazawa/crosspilot/internal/config"
	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// fakeSearcher 记录收到的 spec，并回一份可预期的结果。
type fakeSearcher struct {
	got     catalog.ProductSearchSpec
	calls   int
	result  catalogsearch.SearchResult
	err     error
	lastCtx context.Context
}

func (f *fakeSearcher) Execute(
	ctx context.Context,
	spec catalog.ProductSearchSpec,
) (catalogsearch.SearchResult, error) {
	f.calls++
	f.got = spec
	f.lastCtx = ctx
	return f.result, f.err
}

// resultWith 造一份带一张商品卡的成功结果。
func resultWith() catalogsearch.SearchResult {
	return catalogsearch.SearchResult{
		Hits: []catalogsearch.ProductCard{{
			ProductID:    "P1001",
			Title:        "Roamix 登山背包",
			PriceMajor:   189,
			Currency:     "CNY",
			DefaultSKUID: "P1001-S1",
		}},
		TotalCandidates:  3,
		RecallStrategy:   "keyword",
		ExistenceChecked: true,
	}
}

// decodeResult 把工具结果解成 map，方便按键断言。
func decodeResult(t *testing.T, r tools.ToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Content, &m); err != nil {
		t.Fatalf("结果不是合法 JSON: %v (%s)", err, r.Content)
	}
	return m
}

// TestProductSearchHandler_缺省值补齐 是本文件最重要的一条：schema 里写的
// default 只是给模型的建议，模型省略该字段时 JSON 里根本没有它。
//
// 若不在 handler 里补，TopK 会是 0，而 ProductSearchSpec.Validate 要求
// TopK ∈ [1,100]——每一次不带 top_k 的检索都会以「参数不合法」失败。
func TestProductSearchHandler_缺省值补齐(t *testing.T) {
	t.Parallel()

	fs := &fakeSearcher{result: resultWith()}
	h := newProductSearchHandler(fs)

	if _, err := h(context.Background(),
		json.RawMessage(`{"normalized_query":"登山包"}`)); err != nil {
		t.Fatalf("handler 返回了 Go error（应当只在工具结果里报错）: %v", err)
	}

	if fs.got.TopK != defaultTopK {
		t.Errorf("TopK = %d, 期望缺省补成 %d", fs.got.TopK, defaultTopK)
	}
	if fs.got.TargetCurrency != catalog.Currency(defaultCurrencyCNY) {
		t.Errorf("TargetCurrency = %q, 期望 %s", fs.got.TargetCurrency, defaultCurrencyCNY)
	}
	if fs.got.NormalizedQuery != "登山包" {
		t.Errorf("NormalizedQuery = %q", fs.got.NormalizedQuery)
	}
}

// TestProductSearchHandler_显式参数不被覆盖。
func TestProductSearchHandler_显式参数不被覆盖(t *testing.T) {
	t.Parallel()

	fs := &fakeSearcher{result: resultWith()}
	h := newProductSearchHandler(fs)

	_, _ = h(context.Background(), json.RawMessage(
		`{"normalized_query":"背包","top_k":9,"target_currency":"USD","price_max_major":500,"ship_to":"US"}`))

	if fs.got.TopK != 9 {
		t.Errorf("TopK = %d, 期望 9", fs.got.TopK)
	}
	if fs.got.TargetCurrency != "USD" {
		t.Errorf("TargetCurrency = %q, 期望 USD", fs.got.TargetCurrency)
	}
	if fs.got.PriceMaxMajor == nil || *fs.got.PriceMaxMajor != 500 {
		t.Errorf("PriceMaxMajor = %v, 期望 500", fs.got.PriceMaxMajor)
	}
	if fs.got.ShipTo != "US" {
		t.Errorf("ShipTo = %q", fs.got.ShipTo)
	}
}

// TestProductSearchHandler_结果键名过契约校验 断言返回的键是小写 snake_case。
//
// SearchResult 自身没有 json tag，直接 marshal 会得到 "Hits" / "RecallStrategy"；
// 那样过不了 ToolRequiredFields 的小写校验，而且前端拿不到 hits。
func TestProductSearchHandler_结果键名过契约校验(t *testing.T) {
	t.Parallel()

	h := newProductSearchHandler(&fakeSearcher{result: resultWith()})
	res, err := h(context.Background(), json.RawMessage(`{"normalized_query":"登山包"}`))
	if err != nil {
		t.Fatalf("handler 出错: %v", err)
	}
	if res.State != tools.ResultStateSuccess {
		t.Fatalf("State = %v, 期望 success（%s）", res.State, res.Error)
	}

	// 用工具注册表自己的校验，而不是复述一遍键名——契约只有一处定义。
	if err := tools.NewToolRegistry().ValidateResult(tools.ToolProductSearch, res); err != nil {
		t.Errorf("结果过不了工具契约校验: %v", err)
	}

	m := decodeResult(t, res)
	hits, ok := m["hits"].([]any)
	if !ok || len(hits) != 1 {
		t.Fatalf("hits = %#v, 期望 1 条", m["hits"])
	}
	card, _ := hits[0].(map[string]any)
	if card["product_id"] != "P1001" || card["default_sku_id"] != "P1001-S1" {
		t.Errorf("商品卡字段 = %#v", card)
	}
	if m["recall_strategy"] != "keyword" {
		t.Errorf("recall_strategy = %v", m["recall_strategy"])
	}
}

// TestProductSearchHandler_空命中输出空数组而不是null。
//
// 契约里 hits 是数组；给 null 会让前端与模型都不得不再判一次空。
func TestProductSearchHandler_空命中输出空数组而不是null(t *testing.T) {
	t.Parallel()

	h := newProductSearchHandler(&fakeSearcher{result: catalogsearch.SearchResult{RecallStrategy: "keyword"}})
	res, _ := h(context.Background(), json.RawMessage(`{"normalized_query":"不存在的东西"}`))

	if !strings.Contains(string(res.Content), `"hits":[]`) {
		t.Errorf("空命中应输出 []，实际 %s", res.Content)
	}
	// 契约校验同样要过：hits 键必须存在。
	if err := tools.NewToolRegistry().ValidateResult(tools.ToolProductSearch, res); err != nil {
		t.Errorf("空结果过不了契约校验: %v", err)
	}
}

// TestProductSearchHandler_参数错误回给模型而不是打死这一轮。
//
// 返回非 nil 的 Go error 会让 orchestrator 中止整个 run；而「参数写错了」
// 恰恰是模型能自己改的——必须当成工具结果喂回去。
func TestProductSearchHandler_参数错误回给模型而不是打死这一轮(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args string
	}{
		{"参数不是合法 JSON", `{不是json`},
		{"既无检索词也无商品标识", `{}`},
		{"只有空白检索词", `{"normalized_query":"   "}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := &fakeSearcher{result: resultWith()}
			h := newProductSearchHandler(fs)

			res, err := h(context.Background(), json.RawMessage(tc.args))
			if err != nil {
				t.Fatalf("业务错误不应返回 Go error（会让整轮 run 中止）: %v", err)
			}
			if res.State != tools.ResultStateError {
				t.Errorf("State = %v, 期望 error", res.State)
			}
			if res.Error == "" {
				t.Error("错误结果必须带可读原因，否则模型无从纠正")
			}
			if fs.calls != 0 {
				t.Errorf("参数不合格时不该调用检索（调用了 %d 次）", fs.calls)
			}
		})
	}
}

// TestProductSearchHandler_只给商品ID也能查 断言精确查目录这条入口是通的。
func TestProductSearchHandler_只给商品ID也能查(t *testing.T) {
	t.Parallel()

	fs := &fakeSearcher{result: resultWith()}
	h := newProductSearchHandler(fs)

	res, _ := h(context.Background(), json.RawMessage(`{"product_id":"P1001","sku_id":"P1001-S1"}`))
	if res.State != tools.ResultStateSuccess {
		t.Fatalf("State = %v（%s）", res.State, res.Error)
	}
	if fs.got.ProductID != "P1001" || fs.got.SkuID != "P1001-S1" {
		t.Errorf("spec = %+v", fs.got)
	}
}

// TestProductSearchHandler_检索失败也回给模型。
func TestProductSearchHandler_检索失败也回给模型(t *testing.T) {
	t.Parallel()

	fs := &fakeSearcher{err: errors.New("账本查询超时")}
	h := newProductSearchHandler(fs)

	res, err := h(context.Background(), json.RawMessage(`{"normalized_query":"登山包"}`))
	if err != nil {
		t.Fatalf("检索失败不应返回 Go error: %v", err)
	}
	if res.State != tools.ResultStateError {
		t.Errorf("State = %v, 期望 error", res.State)
	}
	if !strings.Contains(res.Error, "账本查询超时") {
		t.Errorf("错误原因应原样转述给模型: %q", res.Error)
	}
}

// TestToolDefs_只暴露已注册handler的工具 是本设计的关键不变量。
//
// 把没有 handler 的工具写进声明，模型会照着去调，然后每步都拿到
// unknown tool——日志里看起来只是「模型不听话」，实际是装配漏了。
func TestToolDefs_只暴露已注册handler的工具(t *testing.T) {
	t.Parallel()

	exec := tools.NewExecutor()
	exec.Register(tools.ToolProductSearch, newProductSearchHandler(&fakeSearcher{}))

	defs := toolDefs(exec)

	if len(defs) != 1 {
		t.Fatalf("声明了 %d 个工具, 期望 1：%+v", len(defs), defs)
	}
	if defs[0].Name != tools.ToolProductSearch {
		t.Errorf("工具名 = %q", defs[0].Name)
	}

	// 参数必须是合法 JSON Schema：模型端点会直接校验它。
	var schema map[string]any
	if err := json.Unmarshal(defs[0].Parameters, &schema); err != nil {
		t.Fatalf("参数 schema 不是合法 JSON: %v (%s)", err, defs[0].Parameters)
	}
	if schema["type"] != "object" {
		t.Errorf("schema.type = %v, 期望 object", schema["type"])
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema 缺 properties: %v", schema)
	}
	// 提示词里教模型用的那两个参数必须在 schema 里，否则模型传了也会被拒。
	for _, want := range []string{"normalized_query", "top_k", "target_currency"} {
		if _, ok := props[want]; !ok {
			t.Errorf("schema 缺参数 %s", want)
		}
	}
}

// TestToolDefs_无handler时为空 断言装配漏注册时不会静默放行。
func TestToolDefs_无handler时为空(t *testing.T) {
	t.Parallel()

	defs := toolDefs(tools.NewExecutor())
	if len(defs) != 0 {
		t.Errorf("没有注册任何 handler 却声明了 %d 个工具: %+v", len(defs), defs)
	}
}

// TestBuildDecisionProvider_未配置时退回unavailableModel 并验证错误可被
// errors.Is 命中——上层就是靠它决定回 503 还是 500。
func TestBuildDecisionProvider_未配置时退回unavailableModel(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	cases := []struct {
		name string
		cfg  *config.Config
	}{
		{
			name: "选中的 provider 不存在",
			cfg:  &config.Config{LLM: config.LLMConfig{DefaultProvider: "nope", Providers: map[string]config.ProviderConfig{}}},
		},
		{
			name: "密钥为空",
			cfg: &config.Config{LLM: config.LLMConfig{
				DefaultProvider: "qwen",
				Providers: map[string]config.ProviderConfig{
					"qwen": {Name: "qwen", BaseURL: "https://example.invalid/v1", Model: "m"},
				},
			}},
		},
		{
			name: "选中的是 anthropic（协议族未适配）",
			cfg: &config.Config{LLM: config.LLMConfig{
				DefaultProvider: "anthropic",
				Providers: map[string]config.ProviderConfig{
					"anthropic": {Name: "anthropic", BaseURL: "https://api.anthropic.com",
						Model: "claude-sonnet-5-5", APIKey: config.Secret("sk-ant-x")},
				},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, _ := buildDecisionProvider(tc.cfg, logger)
			if _, isLLM := p.(*llm.Client); isLLM {
				t.Fatal("期望退回 unavailableModel，实际构造出了真实适配器")
			}

			_, err := p.Next(context.Background(), protocol.Request{})
			if !errors.Is(err, protocol.ErrModelUnavailable) {
				t.Errorf("err = %v, 期望能被 errors.Is 命中 ErrModelUnavailable", err)
			}
		})
	}
}

// TestBuildDecisionProvider_配置齐备时构造出真实适配器。
func TestBuildDecisionProvider_配置齐备时构造出真实适配器(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{LLM: config.LLMConfig{
		DefaultProvider: "qwen",
		Providers: map[string]config.ProviderConfig{
			"qwen": {
				Name:    "qwen",
				BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
				Model:   "qwen3-max",
				APIKey:  config.Secret("sk-test-not-used"),
			},
		},
	}}

	p, _ := buildDecisionProvider(cfg, slog.New(slog.DiscardHandler))
	if _, ok := p.(*llm.Client); !ok {
		t.Fatalf("期望 *llm.Client，实际 %T", p)
	}
}
