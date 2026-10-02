package main

import (
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

func TestBuildSeedSKUs(t *testing.T) {
	t.Parallel()

	products := []catalog.Product{
		{
			ID:    "P1001",
			Title: "Nomadica 旅行三件套",
			SKUs: []catalog.SKU{
				{ID: "P1001-S1", Spec: "军绿色", Price: catalog.MustMoney("189.00", catalog.CNY), Stock: 50},
				{ID: "P1001-S2", Spec: "沙漠黄", Price: catalog.MustMoney("199.00", catalog.CNY), Stock: 30},
			},
		},
	}

	seeds, skipped, err := buildSeedSKUs(products)
	if err != nil {
		t.Fatalf("buildSeedSKUs 失败: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, 期望 0", skipped)
	}
	if len(seeds) != 2 {
		t.Fatalf("种子数 = %d, 期望 2（每个规格一条）", len(seeds))
	}

	if seeds[0].SKUID != "P1001-S1" || seeds[1].SKUID != "P1001-S2" {
		t.Errorf("SKU 顺序或标识不对: %q %q", seeds[0].SKUID, seeds[1].SKUID)
	}
	if seeds[0].ProductID != "P1001" {
		t.Errorf("ProductID = %q, 期望 P1001", seeds[0].ProductID)
	}
	// 189.00 CNY 是最小单位 18900 分——换算错一位就是 100 倍的价格事故。
	if seeds[0].UnitPriceMinor != 18900 {
		t.Errorf("UnitPriceMinor = %d, 期望 18900", seeds[0].UnitPriceMinor)
	}
	if seeds[1].UnitPriceMinor != 19900 {
		t.Errorf("第二个规格 UnitPriceMinor = %d, 期望 19900（两个规格价格不同，不能被主价格盖掉）",
			seeds[1].UnitPriceMinor)
	}
	if seeds[0].Stock != 50 || seeds[1].Stock != 30 {
		t.Errorf("库存 = %d/%d, 期望 50/30", seeds[0].Stock, seeds[1].Stock)
	}
	if seeds[0].Currency != catalog.CNY {
		t.Errorf("Currency = %q, 期望 CNY", seeds[0].Currency)
	}
	// 标题要带上规格，否则同款两个规格在订单里看起来是同一件商品。
	if seeds[0].Title != "Nomadica 旅行三件套（军绿色）" {
		t.Errorf("Title = %q", seeds[0].Title)
	}
}

func TestBuildSeedSKUs_零小数位币种不补小数(t *testing.T) {
	t.Parallel()

	products := []catalog.Product{{
		ID:    "P2001",
		Title: "日淘好物",
		SKUs: []catalog.SKU{
			{ID: "P2001-S1", Spec: "标准", Price: catalog.MustMoney("2980", catalog.JPY), Stock: 7},
		},
	}}

	seeds, _, err := buildSeedSKUs(products)
	if err != nil {
		t.Fatalf("buildSeedSKUs 失败: %v", err)
	}
	// JPY 没有小数位，2980 日元的最小单位就是 2980，不是 298000。
	if seeds[0].UnitPriceMinor != 2980 {
		t.Errorf("UnitPriceMinor = %d, 期望 2980", seeds[0].UnitPriceMinor)
	}
	if seeds[0].Currency != catalog.JPY {
		t.Errorf("Currency = %q, 期望 JPY", seeds[0].Currency)
	}
}

// TestBuildSeedSKUs_无规格商品被跳过而不是编造库存 断言宁可少导也不编数字：
// 库存种子里 stock 是必填的，凭空给一个数会在下单时变成真实的超卖。
func TestBuildSeedSKUs_无规格商品被跳过而不是编造库存(t *testing.T) {
	t.Parallel()

	products := []catalog.Product{
		{
			ID:    "P3001",
			Title: "有规格",
			SKUs: []catalog.SKU{
				{ID: "P3001-S1", Price: catalog.MustMoney("10.00", catalog.USD), Stock: 3},
			},
		},
		{ID: "P3002", Title: "没有规格明细", SKUs: nil},
	}

	seeds, skipped, err := buildSeedSKUs(products)
	if err != nil {
		t.Fatalf("buildSeedSKUs 失败: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, 期望 1", skipped)
	}
	if len(seeds) != 1 {
		t.Fatalf("种子数 = %d, 期望 1（无规格的商品不产生种子）", len(seeds))
	}
	if seeds[0].ProductID != "P3001" {
		t.Errorf("ProductID = %q, 期望 P3001", seeds[0].ProductID)
	}
}

func TestBuildSeedSKUs_空目录返回空种子(t *testing.T) {
	t.Parallel()

	seeds, skipped, err := buildSeedSKUs(nil)
	if err != nil {
		t.Fatalf("buildSeedSKUs 失败: %v", err)
	}
	if len(seeds) != 0 || skipped != 0 {
		t.Errorf("seeds/skipped = %d/%d, 期望 0/0", len(seeds), skipped)
	}
}
