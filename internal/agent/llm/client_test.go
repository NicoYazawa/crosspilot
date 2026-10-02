package llm_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/agent/llm"
	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
)

// newTestClient 构造一个指向 ts 的客户端。
func newTestClient(t *testing.T, ts *httptest.Server, retries int) *llm.Client {
	t.Helper()
	c, err := llm.New(llm.Config{
		BaseURL:    ts.URL,
		APIKey:     "test-key-should-never-leak",
		Model:      "test-model",
		Timeout:    5 * time.Second,
		MaxRetries: retries,
		Logger:     slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	return c
}

func okResponse(content string, toolCalls string) string {
	tc := "[]"
	if toolCalls != "" {
		tc = toolCalls
	}
	return `{"choices":[{"message":{"content":` + content + `,"tool_calls":` + tc + `},"finish_reason":"stop"}]}`
}

func TestNext_解析文本回复(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, okResponse(`"你好，我来帮你找登山包"`, ""))
	}))
	defer ts.Close()

	resp, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{})
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if resp.Content != "你好，我来帮你找登山包" {
		t.Errorf("Content = %q", resp.Content)
	}
	if len(resp.Calls) != 0 {
		t.Errorf("Calls = %v, 期望空", resp.Calls)
	}
}

// TestNext_解析工具调用 断言 arguments 的二次解析真的发生了。
//
// 协议里 tool_calls[].function.arguments 是一个 JSON **字符串**。若把它当成
// 对象直接解，得到的是零值——模型传的参数会静默丢失，工具拿到空参数。
func TestNext_解析工具调用(t *testing.T) {
	t.Parallel()

	calls := `[{"id":"call_abc","type":"function","function":{` +
		`"name":"product_search_tool","arguments":"{\"normalized_query\":\"登山包\",\"top_k\":3}"}}]`

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, okResponse(`""`, calls))
	}))
	defer ts.Close()

	resp, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{})
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if len(resp.Calls) != 1 {
		t.Fatalf("Calls 数 = %d, 期望 1", len(resp.Calls))
	}
	got := resp.Calls[0]
	if got.ID != "call_abc" || got.Name != "product_search_tool" {
		t.Errorf("调用 = %+v", got)
	}

	var args struct {
		NormalizedQuery string `json:"normalized_query"`
		TopK            int    `json:"top_k"`
	}
	if err := json.Unmarshal(got.Arguments, &args); err != nil {
		t.Fatalf("参数不是合法 JSON: %v (%s)", err, got.Arguments)
	}
	if args.NormalizedQuery != "登山包" || args.TopK != 3 {
		t.Errorf("参数 = %+v, 期望 query=登山包 top_k=3", args)
	}
}

// TestNext_工具参数为空串或null时归一成空对象 断言个别供应商的畸形返回不会
// 让下游解析炸掉。
func TestNext_工具参数为空串或null时归一成空对象(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{`""`, `null`} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			calls := `[{"id":"c1","type":"function","function":{"name":"t","arguments":` + raw + `}}]`
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, okResponse(`""`, calls))
			}))
			defer ts.Close()

			resp, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{})
			if err != nil {
				t.Fatalf("Next 失败: %v", err)
			}
			if string(resp.Calls[0].Arguments) != "{}" {
				t.Errorf("Arguments = %q, 期望 {}", resp.Calls[0].Arguments)
			}
		})
	}
}

// TestNext_content为null时视为空文本（纯工具调用回合就是这样）。
func TestNext_content为null时视为空文本(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, okResponse(`null`, ""))
	}))
	defer ts.Close()

	resp, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{})
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if resp.Content != "" {
		t.Errorf("Content = %q, 期望空串", resp.Content)
	}
}

// TestNext_请求体按协议形状发出 断言 system/tools/tool_choice 与鉴权头。
//
// 这些字段任何一处写错，真实端点都会以 400 拒绝，而单测是唯一能在没有密钥的
// 情况下发现它的地方。
func TestNext_请求体按协议形状发出(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	var gotAuth, gotCT string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, okResponse(`"ok"`, ""))
	}))
	defer ts.Close()

	req := protocol.Request{
		System: "你是购物助手",
		History: []protocol.Message{
			{Role: protocol.RoleUser, Content: "找登山包"},
		},
		Tools: []protocol.ToolDef{{
			Name:        "product_search_tool",
			Description: "检索商品",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		}},
	}
	if _, err := newTestClient(t, ts, 0).Next(context.Background(), req); err != nil {
		t.Fatalf("Next 失败: %v", err)
	}

	if gotAuth != "Bearer test-key-should-never-leak" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotBody["model"] != "test-model" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if gotBody["tool_choice"] != "auto" {
		t.Errorf("tool_choice = %v, 期望 auto", gotBody["tool_choice"])
	}

	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages 数 = %d, 期望 2（system + user）", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是购物助手" {
		t.Errorf("首条消息 = %v, 期望 system 角色", first)
	}

	tools, _ := gotBody["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 数 = %d, 期望 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Errorf("tools[0].type = %v, 期望 function", tool["type"])
	}
}

// TestNext_无参工具也带合法schema 断言省略 Parameters 时不会发出空对象。
func TestNext_无参工具也带合法schema(t *testing.T) {
	t.Parallel()

	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		_, _ = io.WriteString(w, okResponse(`"ok"`, ""))
	}))
	defer ts.Close()

	req := protocol.Request{Tools: []protocol.ToolDef{{Name: "noop", Description: "无参"}}}
	if _, err := newTestClient(t, ts, 0).Next(context.Background(), req); err != nil {
		t.Fatalf("Next 失败: %v", err)
	}

	tools, _ := gotBody["tools"].([]any)
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	params, _ := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Errorf("parameters = %v, 期望含 type=object", fn["parameters"])
	}
}

// TestNext_429重试后成功 断言限流确实被重试而不是直接失败。
func TestNext_429重试后成功(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		_, _ = io.WriteString(w, okResponse(`"终于成功"`, ""))
	}))
	defer ts.Close()

	resp, err := newTestClient(t, ts, 1).Next(context.Background(), protocol.Request{})
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if resp.Content != "终于成功" {
		t.Errorf("Content = %q", resp.Content)
	}
	if calls.Load() != 2 {
		t.Errorf("上游被调用 %d 次, 期望 2", calls.Load())
	}
}

// TestNext_4xx不重试 断言确定性错误不被浪费在重试上。
//
// 401 重试多少次都是 401，只会让调用方多等几个退避周期。
func TestNext_4xx不重试(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid api key"}}`)
	}))
	defer ts.Close()

	_, err := newTestClient(t, ts, 2).Next(context.Background(), protocol.Request{})
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
	if calls.Load() != 1 {
		t.Errorf("上游被调用 %d 次, 期望 1（4xx 不重试）", calls.Load())
	}

	var se *llm.StatusError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Errorf("err = %v, 期望 StatusError(401)", err)
	}
}

// TestNext_重试耗尽后返回最后一次错误。
func TestNext_重试耗尽后返回最后一次错误(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer ts.Close()

	_, err := newTestClient(t, ts, 1).Next(context.Background(), protocol.Request{})
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
	if calls.Load() != 2 {
		t.Errorf("上游被调用 %d 次, 期望 2（1 次 + 1 次重试）", calls.Load())
	}
	// 重试耗尽≠未配置模型：不能包装成 ErrModelUnavailable，否则调用方会收到
	// 一个「去改配置」的 503，而实际上配置是对的、只是上游在抖。
	if errors.Is(err, protocol.ErrModelUnavailable) {
		t.Errorf("上游瞬时故障不应被当成「模型未接入」: %v", err)
	}
}

// TestNext_响应无choices时报错。
func TestNext_响应无choices时报错(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	defer ts.Close()

	if _, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{}); err == nil {
		t.Error("期望返回错误，实际为 nil")
	}
}

// TestNext_200里夹带error字段时报错 断言这种「成功状态码 + 失败内容」被识别。
func TestNext_200里夹带error字段时报错(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"error":{"message":"quota exceeded","type":"limit"}}`)
	}))
	defer ts.Close()

	if _, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{}); err == nil {
		t.Error("期望返回错误，实际为 nil")
	}
}

// TestNext_响应不是合法JSON时报错。
func TestNext_响应不是合法JSON时报错(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>网关错误页</html>")
	}))
	defer ts.Close()

	if _, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{}); err == nil {
		t.Error("期望返回错误，实际为 nil")
	}
}

// TestNext_ctx取消时立即返回 断言取消不会被退避拖住。
func TestNext_ctx取消时立即返回(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 一开始就取消

	start := time.Now()
	// 给足重试次数：若实现无视 ctx，这里会等满所有退避。
	_, err := newTestClient(t, ts, 5).Next(ctx, protocol.Request{})
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("耗时 %v, 期望取消后立即返回", elapsed)
	}
}

// TestStatusError_Timeout 断言瞬时判据（重试与熔断都读它）。
func TestStatusError_Timeout(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		want   bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusBadGateway, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
	}
	for _, c := range cases {
		se := &llm.StatusError{Status: c.status}
		if got := se.Timeout(); got != c.want {
			t.Errorf("StatusError{%d}.Timeout() = %v, 期望 %v", c.status, got, c.want)
		}
	}
}

// TestNew_缺少必填参数时报错 断言装配错误在构造期暴露，而不是等第一次调用。
func TestNew_缺少必填参数时报错(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  llm.Config
	}{
		{"缺 BaseURL", llm.Config{Model: "m"}},
		{"缺 Model", llm.Config{BaseURL: "http://x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := llm.New(tt.cfg); err == nil {
				t.Error("期望返回错误，实际为 nil")
			}
		})
	}
}

// TestClient_错误信息不含密钥 断言明文密钥不会随错误外泄。
//
// 上游的错误体、包装后的 error 都可能出现在日志与响应里，而 Authorization
// 头恰恰是最容易被顺手塞进诊断信息的东西。
func TestClient_错误信息不含密钥(t *testing.T) {
	t.Parallel()

	const secret = "sk-super-secret-key"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad request"}}`)
	}))
	defer ts.Close()

	c, err := llm.New(llm.Config{
		BaseURL: ts.URL, APIKey: secret, Model: "m",
		Timeout: 2 * time.Second, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}

	_, err = c.Next(context.Background(), protocol.Request{})
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("错误信息泄漏了密钥: %v", err)
	}
	if strings.Contains(ts.URL+"/chat/completions", secret) {
		t.Error("测试自身有问题")
	}
}

// TestNext_超长错误体被截断 断言网关的 HTML 错误页不会整段进日志。
func TestNext_超长错误体被截断(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, strings.Repeat("x", 5000))
	}))
	defer ts.Close()

	_, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{})
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
	if len(err.Error()) > 1000 {
		t.Errorf("错误信息长度 %d, 期望被截断", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "已截断") {
		t.Errorf("截断应被标明: %v", err.Error())
	}
}

// TestNext_解析usage 断言 token 用量被采集下来。
//
// 这是成本归因链条的第一环：这一环丢了，后面价格表再对也是全量 unpriced。
func TestNext_解析usage(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"choices":[{"message":{"content":"好的","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{
				"prompt_tokens":1200,
				"completion_tokens":300,
				"prompt_tokens_details":{"cached_tokens":800},
				"completion_tokens_details":{"reasoning_tokens":128}
			}
		}`)
	}))
	defer ts.Close()

	resp, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{})
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if resp.Usage == nil {
		t.Fatal("Usage 为 nil，用量没被采集")
	}
	if resp.Usage.InputTokens != 1200 {
		t.Errorf("InputTokens = %d, 期望 1200", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 300 {
		t.Errorf("OutputTokens = %d, 期望 300", resp.Usage.OutputTokens)
	}
	if resp.Usage.CachedTokens != 800 {
		t.Errorf("CachedTokens = %d, 期望 800", resp.Usage.CachedTokens)
	}
	if resp.Usage.ReasoningTokens != 128 {
		t.Errorf("ReasoningTokens = %d, 期望 128", resp.Usage.ReasoningTokens)
	}
}

// TestNext_深寻式顶层缓存字段 断言 prompt_cache_hit_tokens 也被认。
//
// 同一协议族里缓存命中的字段名并不统一：OpenAI/Qwen/MiniMax 放在
// prompt_tokens_details.cached_tokens 里，DeepSeek 放在顶层。只认其中一种，
// 另一家的缓存命中量会静默变成 0——而 0 的语义是「没有缓存命中」，
// 会让成本算高而不是算低，从数据上看不出是解析漏了。
func TestNext_深寻式顶层缓存字段(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{
			"choices":[{"message":{"content":"好的","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":500,"completion_tokens":100,"prompt_cache_hit_tokens":320}
		}`)
	}))
	defer ts.Close()

	resp, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{})
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if resp.Usage == nil || resp.Usage.CachedTokens != 320 {
		t.Fatalf("CachedTokens = %+v, 期望 320", resp.Usage)
	}
}

// TestNext_无usage块时为零值而非报错 断言上游不返回用量是**正常**情况。
//
// 不少兼容网关不返回 usage。把它当错误会让整轮对话失败；当「用量 0」又会让
// 成本表出现一行「花了 0 元」。正确形态是 Usage 保持 nil，由装配层按 unpriced
// 显式记录（F4 闸门）。
func TestNext_无usage块时为零值而非报错(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, okResponse(`"你好"`, ""))
	}))
	defer ts.Close()

	resp, err := newTestClient(t, ts, 0).Next(context.Background(), protocol.Request{})
	if err != nil {
		t.Fatalf("上游不返回 usage 不该让调用失败: %v", err)
	}
	if resp.Usage != nil {
		t.Errorf("Usage = %+v, 期望 nil（与「用量为 0」区分）", resp.Usage)
	}
}

// TestNext_响应回填provider与model 断言成本归因能知道「谁回答了这次请求」。
//
// 身份随每次响应携带而不是在装配时固化成常量：一旦启用故障转移或 A/B，
// 固化的常量会把 A 家的用量算到 B 家的价目上。
func TestNext_响应回填provider与model(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, okResponse(`"你好"`, ""))
	}))
	defer ts.Close()

	c, err := llm.New(llm.Config{
		BaseURL: ts.URL, APIKey: "k", Provider: "deepseek", Model: "deepseek-flash",
		Timeout: 5 * time.Second, Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("New 失败: %v", err)
	}
	resp, err := c.Next(context.Background(), protocol.Request{})
	if err != nil {
		t.Fatalf("Next 失败: %v", err)
	}
	if resp.Provider != "deepseek" {
		t.Errorf("Provider = %q, 期望 deepseek", resp.Provider)
	}
	if resp.Model != "deepseek-flash" {
		t.Errorf("Model = %q, 期望 deepseek-flash", resp.Model)
	}
}
