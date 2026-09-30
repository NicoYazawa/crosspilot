package catalog

import (
	"fmt"
)

// ProductSearchSpec describes the parameters for a product search.
type ProductSearchSpec struct {
	NormalizedQuery       string
	Category              string
	ShipTo                string
	TopK                  int
	PriceMaxMajor         *float64
	TargetCurrency        Currency
	ExcludedMaterialTags  []string
	RequiredMaterialTags  []string
	ProductID             string
	SkuID                 string
}

// Validate checks that the spec has valid parameters.
func (s ProductSearchSpec) Validate() error {
	if s.TopK < 1 || s.TopK > 100 {
		return fmt.Errorf("%w: TopK must be 1–100, got %d", ErrInvalidProduct, s.TopK)
	}
	if !s.TargetCurrency.Valid() {
		return fmt.Errorf("%w: target currency %q invalid", ErrInvalidProduct, s.TargetCurrency)
	}
	return nil
}
