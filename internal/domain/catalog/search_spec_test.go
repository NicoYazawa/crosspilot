package catalog

import (
	"errors"
	"testing"
)

func TestProductSearchSpecValidate(t *testing.T) {
	valid := ProductSearchSpec{
		NormalizedQuery: "防水登山包",
		TopK:            10,
		TargetCurrency:  CNY,
	}

	if err := valid.Validate(); err != nil {
		t.Errorf("合法 spec 不应报错，得到 %v", err)
	}
}

func TestProductSearchSpecValidateRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ProductSearchSpec)
	}{
		{"TopK 为 0", func(s *ProductSearchSpec) { s.TopK = 0 }},
		{"TopK 为负", func(s *ProductSearchSpec) { s.TopK = -1 }},
		{"TopK 超过 100", func(s *ProductSearchSpec) { s.TopK = 101 }},
		{"TargetCurrency 非法", func(s *ProductSearchSpec) { s.TargetCurrency = Currency("") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(&s)
			if !errors.Is(s.Validate(), ErrInvalidProduct) {
				t.Errorf("应返回 ErrInvalidProduct，得到 %v", s.Validate())
			}
		})
	}
}

func validSpec() ProductSearchSpec {
	return ProductSearchSpec{
		NormalizedQuery: "防水登山包",
		TopK:            10,
		TargetCurrency:  CNY,
	}
}
