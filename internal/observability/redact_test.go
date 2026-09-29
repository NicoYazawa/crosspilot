package observability

import (
	"maps"
	"testing"
)

func TestRedactKeepsOnlyAllowlistedKeys(t *testing.T) {
	attrs := map[string]any{
		"tool":        "search_products",
		"duration_ms": 12,
		"phone":       "13800000000",
		"address":     "某市某区某路 1 号",
	}

	got := Redact(attrs, []string{"tool", "duration_ms"})

	if len(got) != 2 {
		t.Fatalf("结果包含 %d 个字段，期望 2：%v", len(got), got)
	}
	if got["tool"] != "search_products" || got["duration_ms"] != 12 {
		t.Errorf("白名单字段取值不对：%v", got)
	}
	if _, ok := got["phone"]; ok {
		t.Error("未列入白名单的字段不应被保留")
	}
}

func TestRedactMissingAllowlistedKeyIsAbsent(t *testing.T) {
	got := Redact(map[string]any{"tool": "search"}, []string{"tool", "model"})

	if _, ok := got["model"]; ok {
		t.Errorf("入参里没有的字段不应凭空出现：%v", got)
	}
}

func TestRedactEmptyInputs(t *testing.T) {
	cases := []struct {
		name      string
		attrs     map[string]any
		allowlist []string
	}{
		{"空属性", map[string]any{}, []string{"tool"}},
		{"nil 属性", nil, []string{"tool"}},
		{"空白名单", map[string]any{"tool": "x"}, nil},
		{"两者皆空", nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Redact(tc.attrs, tc.allowlist)
			if got == nil {
				t.Fatal("结果不应为 nil，调用方需要能安全地读它")
			}
			if len(got) != 0 {
				t.Errorf("结果应为空：%v", got)
			}
		})
	}
}

func TestRedactDoesNotMutateInput(t *testing.T) {
	attrs := map[string]any{"tool": "search", "phone": "13800000000"}
	before := maps.Clone(attrs)

	Redact(attrs, []string{"tool"})

	if !maps.Equal(attrs, before) {
		t.Errorf("入参被修改：%v", attrs)
	}
}

func TestAnySensitive(t *testing.T) {
	sensitive := []string{"phone", "address", "email"}

	cases := []struct {
		name  string
		attrs map[string]any
		want  bool
	}{
		{"命中", map[string]any{"tool": "search", "phone": "13800000000"}, true},
		{"未命中", map[string]any{"tool": "search"}, false},
		{"命中但为空值", map[string]any{"phone": nil}, false},
		{"空表", map[string]any{}, false},
		{"无敏感名单", map[string]any{"phone": "138"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list := sensitive
			if tc.name == "无敏感名单" {
				list = nil
			}
			if got := AnySensitive(tc.attrs, list); got != tc.want {
				t.Errorf("AnySensitive = %v，期望 %v", got, tc.want)
			}
		})
	}
}
