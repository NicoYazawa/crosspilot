package catalog

import (
	"errors"
	"testing"
)

func validProduct() Product {
	return Product{
		ID:          "p-001",
		Title:       "无线降噪耳机",
		Description: "主动降噪，续航 30 小时",
		CategoryID:  "audio",
		Price:       MustMoney("1299.00", CNY),
		ImageURL:    "https://example.invalid/p-001.png",
		Rating:      4.6,
		ReviewCount: 128,
		InStock:     true,
		Attributes:  map[string]string{"颜色": "黑"},
		Tags:        []string{"耳机", "降噪"},
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
		{"币种非法", func(p *Product) { p.Price.Currency = "cny" }},
		{"价格为负", func(p *Product) { p.Price = MustMoney("-1.00", CNY) }},
		{"评分越界上", func(p *Product) { p.Rating = 5.1 }},
		{"评分越界下", func(p *Product) { p.Rating = -0.1 }},
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

func TestProductSearchSpecNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"零值取默认", 0, DefaultSearchLimit},
		{"负值取默认", -5, DefaultSearchLimit},
		{"超上限被截断", 10_000, MaxSearchLimit},
		{"正常值保留", 30, 30},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ProductSearchSpec{Limit: tc.in}.Normalize()
			if got.Limit != tc.want {
				t.Errorf("Limit %d 归一为 %d，期望 %d", tc.in, got.Limit, tc.want)
			}
		})
	}
}

func TestProductSearchSpecNormalizeDoesNotMutate(t *testing.T) {
	original := ProductSearchSpec{Limit: 0}
	if got := original.Normalize(); got.Limit == original.Limit {
		t.Error("Normalize 应返回补齐后的副本")
	}
	if original.Limit != 0 {
		t.Error("Normalize 不应修改原值")
	}
}

func TestProductSearchSpecValidate(t *testing.T) {
	low := MustMoney("10.00", USD)
	high := MustMoney("20.00", USD)

	if err := (ProductSearchSpec{MinPrice: &low, MaxPrice: &high}).Validate(); err != nil {
		t.Errorf("合法价格区间不应报错，得到 %v", err)
	}

	// 边界相等是允许的
	if err := (ProductSearchSpec{MinPrice: &high, MaxPrice: &high}).Validate(); err != nil {
		t.Errorf("上下界相等应允许，得到 %v", err)
	}
}

func TestProductSearchSpecValidateRejectsBadInput(t *testing.T) {
	low := MustMoney("10.00", USD)
	high := MustMoney("20.00", USD)
	eur := MustMoney("5.00", EUR)

	cases := []struct {
		name string
		spec ProductSearchSpec
	}{
		{"下界高于上界", ProductSearchSpec{MinPrice: &high, MaxPrice: &low}},
		{"币种不一致", ProductSearchSpec{MinPrice: &low, MaxPrice: &eur}},
		{"条数为负", ProductSearchSpec{Limit: -1}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.spec.Validate(); !errors.Is(err, ErrInvalidSearchSpec) {
				t.Errorf("应返回 ErrInvalidSearchSpec，得到 %v", err)
			}
		})
	}
}

func TestProductSearchSpecZeroValueIsValid(t *testing.T) {
	var spec ProductSearchSpec
	if err := spec.Validate(); err != nil {
		t.Errorf("零值检索条件应合法，得到 %v", err)
	}
	if got := spec.Normalize().Limit; got != DefaultSearchLimit {
		t.Errorf("零值归一后 Limit = %d，期望 %d", got, DefaultSearchLimit)
	}
}
