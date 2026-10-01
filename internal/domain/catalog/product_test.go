package catalog

import (
	"errors"
	"testing"
)

func validProduct() Product {
	return Product{
		ID:            "P0001",
		Title:         "无线降噪耳机",
		Description:   "主动降噪，续航 30 小时",
		Category:      "数码配件",
		Brand:         "AudioTech",
		OriginCountry: "CN",
		InStock:       true,
		ImageURL:      "https://example.invalid/p-001.png",
		WeightKg:      0.25,
		DimensionsCm:  map[string]float64{"length": 20, "width": 18, "height": 8},
		PrimaryPrice:  MustMoney("1299.00", CNY),
		SKUs: []SKU{
			{ID: "P0001-S1", Spec: "黑色", Price: MustMoney("1299.00", CNY), Stock: 10},
		},
		DefaultSKUID:   "P0001-S1",
		ShipsTo:        []string{"CN", "US", "EU"},
		MaterialTags:   []string{"塑料", "金属"},
		Highlights:     []string{"主动降噪", "30小时续航"},
		Tags:           []string{"耳机", "降噪"},
		CanonicalID:    "CANON-P0001",
		SourcePlatform: "PlatformA",
		RatingSummary:  map[string]float64{"average": 4.6, "review_count": 128},
		RatingIsLive:   true,
		UpdatedAt:      "2024-01-01T00:00:00Z",
		SourceLanguage: "zh",
		SourceLocale:   "zh-CN",
		DataProvenance: "imported",
		Attributes:     map[string]string{"颜色": "黑"},
	}
}

func TestProductValidateAcceptsWellFormed(t *testing.T) {
	if err := validProduct().Validate(); err != nil {
		t.Errorf("合法商品不应报错，得到 %v", err)
	}
}

func TestProductValidateRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Product)
	}{
		{"ID 为空", func(p *Product) { p.ID = "" }},
		{"标题为空", func(p *Product) { p.Title = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProduct()
			tc.mutate(&p)
			if err := p.Validate(); !errors.Is(err, ErrInvalidProduct) {
				t.Errorf("应返回 ErrInvalidProduct，得到 %v", err)
			}
		})
	}
}

func TestProductPrice(t *testing.T) {
	// 无 SKU 时返回 PrimaryPrice
	p := Product{ID: "P1", Title: "Test", PrimaryPrice: MustMoney("100.00", CNY)}
	if got := p.Price(); got.Amount.String() != "100.00" {
		t.Errorf("Price() = %s, want 100.00", got.Amount.String())
	}

	// 有库存 SKU 时返回有库存的 SKU 价格
	p2 := Product{
		ID:    "P2",
		Title: "Test2",
		SKUs: []SKU{
			{ID: "P2-S1", Spec: "A", Price: MustMoney("50.00", CNY), Stock: 0},
			{ID: "P2-S2", Spec: "B", Price: MustMoney("75.00", CNY), Stock: 5},
		},
		DefaultSKUID: "P2-S1",
	}
	if got := p2.Price(); got.Amount.String() != "75.00" {
		t.Errorf("Price() = %s, want 75.00", got.Amount.String())
	}
}

func TestProductPrimaryAvailableSKU(t *testing.T) {
	// 优先默认规格
	p := Product{
		SKUs: []SKU{
			{ID: "P-S1", Spec: "A", Stock: 0},
			{ID: "P-S2", Spec: "B", Stock: 3},
		},
		DefaultSKUID: "P-S2",
	}
	if got := p.PrimaryAvailableSKU(); got.ID != "P-S2" {
		t.Errorf("PrimaryAvailableSKU() = %s, want P-S2", got.ID)
	}

	// 默认规格无库存时选有库存的第一个
	p2 := Product{
		SKUs: []SKU{
			{ID: "P-S1", Spec: "A", Stock: 0},
			{ID: "P-S2", Spec: "B", Stock: 3},
		},
		DefaultSKUID: "P-S1",
	}
	if got := p2.PrimaryAvailableSKU(); got.ID != "P-S2" {
		t.Errorf("PrimaryAvailableSKU() = %s, want P-S2", got.ID)
	}

	// 都无库存返回第一个
	p3 := Product{
		SKUs: []SKU{
			{ID: "P-S1", Spec: "A", Stock: 0},
			{ID: "P-S2", Spec: "B", Stock: 0},
		},
	}
	if got := p3.PrimaryAvailableSKU(); got.ID != "P-S1" {
		t.Errorf("PrimaryAvailableSKU() = %s, want P-S1", got.ID)
	}
}

func TestProductHasAvailableSKU(t *testing.T) {
	p := Product{
		SKUs: []SKU{
			{ID: "P-S1", Stock: 0},
			{ID: "P-S2", Stock: 5},
		},
	}
	if !p.HasAvailableSKU() {
		t.Error("HasAvailableSKU() = false, want true")
	}

	p2 := Product{
		SKUs: []SKU{
			{ID: "P-S1", Stock: 0},
			{ID: "P-S2", Stock: 0},
		},
	}
	if p2.HasAvailableSKU() {
		t.Error("HasAvailableSKU() = true, want false")
	}
}

func TestProductSearchableText(t *testing.T) {
	p := Product{
		Brand:       "AudioTech",
		Title:       "无线耳机",
		Category:    "数码配件",
		Description: "蓝牙降噪",
		SKUs: []SKU{
			{ID: "P-S1", Spec: "黑色"},
		},
	}
	text := p.SearchableText()
	expected := "AudioTech 无线耳机 数码配件 蓝牙降噪 黑色"
	if text != expected {
		t.Errorf("SearchableText() = %q, want %q", text, expected)
	}
}

func TestSKUToSKUInfo(t *testing.T) {
	s := SKU{
		ID:    "P-S1",
		Spec:  "黑色",
		Price: MustMoney("299.00", CNY),
		Stock: 15,
	}
	info := s.ToSKUInfo()
	if info.SKUID != "P-S1" {
		t.Errorf("SKUID = %s, want P-S1", info.SKUID)
	}
	if info.Spec != "黑色" {
		t.Errorf("Spec = %s, want 黑色", info.Spec)
	}
	if info.PriceMajor != 299.00 {
		t.Errorf("PriceMajor = %f, want 299.00", info.PriceMajor)
	}
	if info.Currency != "CNY" {
		t.Errorf("Currency = %s, want CNY", info.Currency)
	}
	if info.Stock != 15 {
		t.Errorf("Stock = %d, want 15", info.Stock)
	}
}
