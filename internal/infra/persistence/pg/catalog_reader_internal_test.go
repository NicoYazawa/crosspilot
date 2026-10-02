package pg

import (
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// 本文件测的是 orderableSKUs 这个纯函数，和同目录下的集成用例分开：
// 规格回退规则是这层最容易悄悄退化的地方（退回单规格时不会报错，只会少一个
// 可下单调规格），值得用不依赖数据库的用例把每一条回退路径钉死。

func TestOrderableSKUs_还原全部规格(t *testing.T) {
	t.Parallel()

	attrs := []byte(`{"skus":[
		{"sku_id":"P1-S1","spec":"军绿","currency":"CNY","price_major":189.0,"stock":50},
		{"sku_id":"P1-S2","spec":"沙漠黄","currency":"CNY","price_major":199.0,"stock":30}
	]}`)

	got := orderableSKUs(attrs, catalog.CNY, "P1-S1", catalog.MustMoney("189.00", catalog.CNY))

	if len(got) != 2 {
		t.Fatalf("规格数 = %d, 期望 2（attributes 里有几个就要返回几个）", len(got))
	}
	if got[1].SKUID != "P1-S2" || got[1].Spec != "沙漠黄" {
		t.Errorf("第二个规格 = %+v, 期望 P1-S2/沙漠黄", got[1])
	}
	if got[1].Price.String() != "199.00 CNY" {
		t.Errorf("第二个规格价格 = %q, 期望 199.00 CNY", got[1].Price.String())
	}
	if got[1].Stock != 30 {
		t.Errorf("第二个规格库存 = %d, 期望 30", got[1].Stock)
	}
}

// TestOrderableSKUs_属性缺席或坏掉时退回默认规格 断言兜底那条不是编造：
// SKUID 与价格都取自库里的权威列（default_sku_id / price_amount_major）。
func TestOrderableSKUs_属性缺席或坏掉时退回默认规格(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		attrs []byte
	}{
		{"属性列为 NULL", nil},
		{"属性为空对象", []byte(`{}`)},
		{"skus 是空数组", []byte(`{"skus":[]}`)},
		{"skus 不是数组", []byte(`{"skus":"P1-S1"}`)},
		{"skus 元素缺 sku_id", []byte(`{"skus":[{"price_major":1,"stock":1}]}`)},
		{"skus 元素价格解不出", []byte(`{"skus":[{"sku_id":"P1-S1","price_major":"很贵"}]}`)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := orderableSKUs(tt.attrs, catalog.CNY, "P1-S1", catalog.MustMoney("189.00", catalog.CNY))

			if len(got) != 1 {
				t.Fatalf("规格数 = %d, 期望 1（退回单规格）", len(got))
			}
			if got[0].SKUID != "P1-S1" {
				t.Errorf("SKUID = %q, 期望沿用库里的 default_sku_id", got[0].SKUID)
			}
			if got[0].Price.String() != "189.00 CNY" {
				t.Errorf("价格 = %q, 期望沿用库里的 price_amount_major", got[0].Price.String())
			}
		})
	}
}

// TestOrderableSKUs_属性坏掉不算商品坏掉 断言降级边界：一件商品的两个规格里
// 有一个坏掉时整件商品退回单规格，而不是让下单整体失败——库里权威的价格与默认
// 规格还在，这件商品依然是可下单的。
func TestOrderableSKUs_属性坏掉不算商品坏掉(t *testing.T) {
	t.Parallel()

	got := orderableSKUs(
		[]byte(`{"skus":[{"sku_id":"P1-S1","price_major":189.0,"stock":1},{"sku_id":"P1-S2"}]}`),
		catalog.CNY, "P1-S1", catalog.MustMoney("189.00", catalog.CNY),
	)

	if len(got) != 1 || got[0].SKUID != "P1-S1" {
		t.Fatalf("希望退回默认规格 P1-S1，实际 %+v", got)
	}
}
