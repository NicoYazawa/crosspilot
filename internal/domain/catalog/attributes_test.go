package catalog_test

import (
	"encoding/json"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

func TestSKUsFromAttributes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// attrs 直接写成 JSON 字面量：用例要验证的正是「JSONB 读回来的形状」，
		// 手工构造 map 会绕开 json.Unmarshal 真正产生的那种类型。
		attrsJSON string
		wantLen   int
		check     func(t *testing.T, skus []catalog.SKU)
	}{
		{
			name:      "两个规格各自带价与库存",
			attrsJSON: `{"skus":[{"sku_id":"P1-S1","spec":"军绿","currency":"CNY","price_major":189.0,"stock":50},{"sku_id":"P1-S2","spec":"沙漠黄","currency":"CNY","price_major":199.0,"stock":30}]}`,
			wantLen:   2,
			check: func(t *testing.T, skus []catalog.SKU) {
				if skus[0].ID != "P1-S1" || skus[0].Spec != "军绿" || skus[0].Stock != 50 {
					t.Errorf("第一个规格 = %+v", skus[0])
				}
				// 199.0 走 float64→最短表示→decimal，不能变成 199.00000000000003
				if got := skus[1].Price.String(); got != "199.00 CNY" {
					t.Errorf("第二个规格价格 = %q, 期望 199.00 CNY", got)
				}
				if skus[1].Stock != 30 {
					t.Errorf("第二个规格库存 = %d, 期望 30", skus[1].Stock)
				}
			},
		},
		{
			name:      "规格自带币种优先于商品主币种",
			attrsJSON: `{"skus":[{"sku_id":"P2-S1","currency":"JPY","price_major":2980,"stock":1}]}`,
			wantLen:   1,
			check: func(t *testing.T, skus []catalog.SKU) {
				if skus[0].Price.Currency != catalog.JPY {
					t.Errorf("币种 = %q, 期望 JPY", skus[0].Price.Currency)
				}
				if got := skus[0].Price.String(); got != "2980 JPY" {
					t.Errorf("价格 = %q, 期望 2980 JPY（零小数位币种不补小数）", got)
				}
			},
		},
		{
			name:      "规格缺币种时退回商品主币种",
			attrsJSON: `{"skus":[{"sku_id":"P3-S1","price_major":12.5,"stock":4}]}`,
			wantLen:   1,
			check: func(t *testing.T, skus []catalog.SKU) {
				// 传进来的 fallback 是 USD。
				if skus[0].Price.Currency != catalog.USD {
					t.Errorf("币种 = %q, 期望退回 USD", skus[0].Price.Currency)
				}
			},
		},
		{
			name:      "没有 skus 键表示没有规格明细",
			attrsJSON: `{"highlights":[{"label":"材质","detail":"帆布"}]}`,
			wantLen:   0,
		},
		{
			name:      "skus 为空数组同样表示没有规格明细",
			attrsJSON: `{"skus":[]}`,
			wantLen:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			attrs, err := catalog.AttributesFromJSON([]byte(tt.attrsJSON))
			if err != nil {
				t.Fatalf("解析属性失败: %v", err)
			}

			skus, err := catalog.SKUsFromAttributes(attrs, catalog.USD)
			if err != nil {
				t.Fatalf("SKUsFromAttributes 失败: %v", err)
			}
			if len(skus) != tt.wantLen {
				t.Fatalf("规格数 = %d, 期望 %d", len(skus), tt.wantLen)
			}
			if tt.check != nil {
				tt.check(t, skus)
			}
		})
	}
}

// TestSKUsFromAttributes_坏数据必须报错 断言「没有规格」与「规格是坏的」没被混为一谈。
//
// 混为一谈的后果是静默降级：一行损坏的数据会被当成一件「没有规格明细」的正常
// 商品，调用方退回单规格继续跑，而少掉的规格永远不会有人发现。
func TestSKUsFromAttributes_坏数据必须报错(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		attrsJSON string
	}{
		{"skus 不是数组", `{"skus":{"sku_id":"S1"}}`},
		{"数组元素不是对象", `{"skus":["P1-S1"]}`},
		{"元素缺 sku_id", `{"skus":[{"spec":"军绿","price_major":1,"stock":1}]}`},
		{"元素价格缺失", `{"skus":[{"sku_id":"P1-S1","stock":1}]}`},
		{"元素价格不是数字", `{"skus":[{"sku_id":"P1-S1","price_major":"很贵","stock":1}]}`},
		{"元素币种非法", `{"skus":[{"sku_id":"P1-S1","currency":"XX","price_major":1,"stock":1}]}`},
		{"第二个元素坏掉也要报错", `{"skus":[{"sku_id":"P1-S1","price_major":1,"stock":1},{"sku_id":""}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			attrs, err := catalog.AttributesFromJSON([]byte(tt.attrsJSON))
			if err != nil {
				t.Fatalf("解析属性失败: %v", err)
			}
			if _, err := catalog.SKUsFromAttributes(attrs, catalog.CNY); err == nil {
				t.Error("期望返回错误，实际为 nil")
			}
		})
	}
}

func TestAttributesFromJSON(t *testing.T) {
	t.Parallel()

	if attrs, err := catalog.AttributesFromJSON(nil); err != nil || attrs != nil {
		t.Errorf("空输入 = %v, %v; 期望 nil, nil（这一列可以为空）", attrs, err)
	}

	if _, err := catalog.AttributesFromJSON([]byte(`{不是 JSON`)); err == nil {
		t.Error("畸形 JSON 期望返回错误，实际为 nil")
	}

	attrs, err := catalog.AttributesFromJSON([]byte(`{"tax_category":"旅行装备","skus":[]}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if attrs["tax_category"] != "旅行装备" {
		t.Errorf("tax_category = %v", attrs["tax_category"])
	}
}

func TestMoneyFromJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		v    any
		want string
	}{
		{"float64 用最短表示", float64(189.0), "189.00 CNY"},
		{"float64 带小数", float64(12.5), "12.50 CNY"},
		{"json.Number 保留字面量", json.Number("199.0"), "199.00 CNY"},
		{"字符串", "29.99", "29.99 CNY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			m, err := catalog.MoneyFromJSON(tt.v, catalog.CNY)
			if err != nil {
				t.Fatalf("MoneyFromJSON 失败: %v", err)
			}
			if got := m.String(); got != tt.want {
				t.Errorf("金额 = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

func TestMoneyFromJSON_不支持的输入报错(t *testing.T) {
	t.Parallel()

	for _, v := range []any{nil, true, []any{1.0}, map[string]any{}} {
		if _, err := catalog.MoneyFromJSON(v, catalog.CNY); err == nil {
			t.Errorf("输入 %#v 期望返回错误，实际为 nil", v)
		}
	}
}
