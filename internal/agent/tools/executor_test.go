package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/agent/fakemodel"
	"github.com/NicoYazawa/crosspilot/internal/agent/protocol"
)

func TestNewExecutor(t *testing.T) {
	e := NewExecutor()
	if e == nil {
		t.Fatal("NewExecutor 返回 nil")
	}
	if e.Has("anything") {
		t.Error("空 executor 不应 Has 任意工具")
	}
}

func TestExecutor_Register(t *testing.T) {
	e := NewExecutor()

	e.Register("test_tool", func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
		return ToolResult{Content: json.RawMessage(`{"ok":true}`), State: ResultStateSuccess}, nil
	})

	if !e.Has("test_tool") {
		t.Error("注册后 Has 应为 true")
	}

	t.Run("重复注册panic", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Error("重复注册应 panic")
			}
		}()
		e.Register("test_tool", func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
			return ToolResult{}, nil
		})
	})
}

func TestExecutor_Has(t *testing.T) {
	e := NewExecutor()
	if e.Has("missing") {
		t.Error("空 executor Has missing 应为 false")
	}

	e.Register("present", func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
		return ToolResult{}, nil
	})
	if !e.Has("present") {
		t.Error("注册后 Has present 应为 true")
	}
}

func TestExecutor_Execute(t *testing.T) {
	e := NewExecutor()

	e.Register("greet", func(_ context.Context, args json.RawMessage) (ToolResult, error) {
		var a struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			t.Fatalf("json.Unmarshal 失败: %v", err)
		}
		return ToolResult{
			Content: json.RawMessage(`{"greeting":"Hello, ` + a.Name + `!"}`),
			State:   ResultStateSuccess,
		}, nil
	})

	t.Run("成功执行", func(t *testing.T) {
		result, err := e.Execute(context.Background(), "greet", json.RawMessage(`{"name":"Alice"}`))
		if err != nil {
			t.Fatalf("Execute 失败: %v", err)
		}
		if result.State != ResultStateSuccess {
			t.Errorf("State = %v, want ResultStateSuccess", result.State)
		}
	})

	t.Run("未知工具", func(t *testing.T) {
		result, err := e.Execute(context.Background(), "unknown_tool", nil)
		if !errors.Is(err, ErrUnknownTool) {
			t.Errorf("应为 ErrUnknownTool，实际 %v", err)
		}
		if result.State != ResultStateError {
			t.Errorf("State = %v, want ResultStateError", result.State)
		}
		if !strings.Contains(result.Error, "unknown tool") {
			t.Errorf("Error = %q, want 含 'unknown tool'", result.Error)
		}
	})

	t.Run("handler返回错误", func(t *testing.T) {
		e.Register("fail_tool", func(_ context.Context, _ json.RawMessage) (ToolResult, error) {
			return ToolResult{State: ResultStateError, Error: "handler failed"}, errors.New("handler error")
		})
		result, err := e.Execute(context.Background(), "fail_tool", nil)
		if err == nil {
			t.Error("handler 返回 error 时 Execute 应返回 error")
		}
		if result.State != ResultStateError {
			t.Errorf("State = %v, want ResultStateError", result.State)
		}
		if result.Error != "handler failed" {
			t.Errorf("Error = %q, want 'handler failed'", result.Error)
		}
	})

	t.Run("端到端fake_model调用", func(t *testing.T) {
		fake := fakemodel.New(
			fakemodel.Response{
				Step: fakemodel.Step{
					Content: "请帮我搜索商品",
					Calls: []fakemodel.ToolCall{
						{Name: "product_search_tool", Arguments: fakemodel.MustArgsRaw(`{"query":"防水登山包"}`)},
					},
				},
			},
			fakemodel.Response{
				Step: fakemodel.Step{
					Content: "找到了3件商品",
				},
			},
		)

		e.Register("product_search_tool", func(_ context.Context, args json.RawMessage) (ToolResult, error) {
			var a struct {
				Query string `json:"query"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				t.Fatalf("json.Unmarshal 失败: %v", err)
			}
			return ToolResult{
				Content: json.RawMessage(`{"hits":[{"name":"` + a.Query + `"}],"recall_strategy":"embedding_only"}`),
				State:   ResultStateSuccess,
			}, nil
		})

		resp, err := fake.Next(context.Background(), protocol.Request{})
		if err != nil {
			t.Fatalf("fake.Next 失败: %v", err)
		}
		if len(resp.Calls) == 0 {
			t.Fatal("resp.Calls 为空")
		}
		result, err := e.Execute(context.Background(), resp.Calls[0].Name, resp.Calls[0].Arguments)
		if err != nil {
			t.Fatalf("Execute 失败: %v", err)
		}
		if result.State != ResultStateSuccess {
			t.Errorf("State = %v, want ResultStateSuccess", result.State)
		}
	})
}

func TestExecutor_DecodeArgs(t *testing.T) {
	t.Run("成功解码", func(t *testing.T) {
		raw := json.RawMessage(`{"name":"Bob","age":30}`)
		args, err := DecodeArgs[map[string]any](raw)
		if err != nil {
			t.Fatalf("DecodeArgs 失败: %v", err)
		}
		if args["name"] != "Bob" {
			t.Errorf("name = %v, want Bob", args["name"])
		}
		if args["age"] != float64(30) {
			t.Errorf("age = %v, want 30.0", args["age"])
		}
	})

	t.Run("JSON格式错误", func(t *testing.T) {
		_, err := DecodeArgs[map[string]any](json.RawMessage(`{invalid`))
		if err == nil {
			t.Error("非法 JSON 应报错")
		}
	})
}
