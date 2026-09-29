package catalog

import "fmt"

// Product 是目录中的一件商品。
//
// 这是领域层的读模型：检索与展示都从它出发，它不关心自己来自关系库还是向量库。
type Product struct {
	ID          string
	Title       string
	Description string
	CategoryID  string
	Price       Money
	ImageURL    string
	Rating      float64 // 0–5 分，非金额，不参与账务累加
	ReviewCount int
	InStock     bool
	Attributes  map[string]string
	Tags        []string
}

// Validate 报告商品是否满足领域约束。
func (p Product) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("%w: ID 为空", ErrInvalidProduct)
	}
	if p.Title == "" {
		return fmt.Errorf("%w: 商品 %s 标题为空", ErrInvalidProduct, p.ID)
	}
	if !p.Price.Currency.Valid() {
		return fmt.Errorf("%w: 商品 %s 币种非法 %q", ErrInvalidProduct, p.ID, string(p.Price.Currency))
	}
	if p.Price.IsNegative() {
		return fmt.Errorf("%w: 商品 %s 价格为负", ErrInvalidProduct, p.ID)
	}
	if p.Rating < 0 || p.Rating > 5 {
		return fmt.Errorf("%w: 商品 %s 评分越界 %v", ErrInvalidProduct, p.ID, p.Rating)
	}
	return nil
}
