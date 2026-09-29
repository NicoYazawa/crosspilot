package catalog

import "fmt"

const (
	// DefaultSearchLimit 是未指定条数时返回的结果条数。
	DefaultSearchLimit = 20

	// MaxSearchLimit 是单次检索允许返回的最大条数，用于挡住失控的全表拉取。
	MaxSearchLimit = 100
)

// ProductSearchSpec 描述一次商品检索的筛选条件。
//
// 零值可用：Query 为空即按条件浏览，Limit 为 0 会归一为 DefaultSearchLimit。
type ProductSearchSpec struct {
	Query      string
	CategoryID string
	Tags       []string
	MinPrice   *Money // 含端点，nil 表示不限
	MaxPrice   *Money // 含端点，nil 表示不限
	Limit      int
	Cursor     string
}

// Normalize 返回补齐默认值后的副本，不修改原值。
func (s ProductSearchSpec) Normalize() ProductSearchSpec {
	out := s
	if out.Limit <= 0 {
		out.Limit = DefaultSearchLimit
	}
	if out.Limit > MaxSearchLimit {
		out.Limit = MaxSearchLimit
	}
	return out
}

// Validate 报告检索条件是否自洽。
func (s ProductSearchSpec) Validate() error {
	if s.MinPrice != nil && s.MaxPrice != nil {
		if s.MinPrice.Currency != s.MaxPrice.Currency {
			return fmt.Errorf("%w: 价格区间币种不一致 %s / %s",
				ErrInvalidSearchSpec, s.MinPrice.Currency, s.MaxPrice.Currency)
		}
		// 币种已确认一致，直接比数值，无需再走会返回错误的 Cmp
		if s.MinPrice.Amount.Cmp(s.MaxPrice.Amount) > 0 {
			return fmt.Errorf("%w: 价格下界 %s 高于上界 %s",
				ErrInvalidSearchSpec, s.MinPrice, s.MaxPrice)
		}
	}
	if s.Limit < 0 {
		return fmt.Errorf("%w: 条数不能为负: %d", ErrInvalidSearchSpec, s.Limit)
	}
	return nil
}
