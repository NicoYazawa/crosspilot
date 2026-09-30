package catalog

import (
	"fmt"
)

// Product 是目录中的一件商品。
//
// 领域层的读模型：检索与展示都从它出发，它不关心自己来自关系库还是向量库。
// 包含商品卡展示所需的全部字段（对比 P0 骨架版扩展了展示相关字段）。
type Product struct {
	ID          string
	Title       string
	Description string
	Category    string // 品类，如"旅行装备"、"数码配件"
	Brand       string
	OriginCountry string
	InStock     bool
	ImageURL    string
	ImageKind   string // "placeholder" | "product" | ...
	ImageAlt    string
	WeightKg    float64
	DimensionsCm map[string]float64 // length/width/height

	// 价格与规格
	PrimaryPrice Money       // 主规格的参考价（用于无 SPEC 查询时的展示）
	SKUs         []SKU       // 全部规格
	DefaultSKUID string      // 默认选中的规格 ID

	// 跨境属性
	ShipsTo      []string    // 可送达国家列表，如 ["CN", "US", "EU"]
	MaterialTags []string    // 材质标签，用于过滤

	// 元数据
	Highlights     []string           // 核心卖点，如 [{"label":"材质","detail":"帆布+再生尼龙"}]
	Tags           []string
	CanonicalID    string             // 同款合并ID
	SourcePlatform string             // 来源平台
	RatingSummary  map[string]float64 // {"average": 4.5, "review_count": 128}
	RatingIsLive   bool               // 评分是否实时
	UpdatedAt      string             // ISO8601 时间戳
	SourceLanguage string
	SourceLocale   string
	DataProvenance string
	Attributes     map[string]string  // 其他属性键值对
}

// Validate 报告商品是否满足领域约束。
func (p Product) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("%w: ID 为空", ErrInvalidProduct)
	}
	if p.Title == "" {
		return fmt.Errorf("%w: 商品 %s 标题为空", ErrInvalidProduct, p.ID)
	}
	if !p.PrimaryPrice.Currency.Valid() && !p.Price().Currency.Valid() {
		// 如果有 SKU 价格也行
		hasPrice := false
		for _, sku := range p.SKUs {
			if sku.Price.Currency.Valid() {
				hasPrice = true
				break
			}
		}
		if !hasPrice {
			return fmt.Errorf("%w: 商品 %s 无有效价格", ErrInvalidProduct, p.ID)
		}
	}
	return nil
}

// Price 返回商品的主要参考价（优先用主规格价格，其次用 PrimaryPrice）。
func (p Product) Price() Money {
	if len(p.SKUs) > 0 {
		for _, sku := range p.SKUs {
			if sku.Stock > 0 && sku.Price.Currency.Valid() {
				return sku.Price
			}
		}
		return p.SKUs[0].Price
	}
	return p.PrimaryPrice
}

// PrimaryAvailableSKU 返回有库存的主规格。
func (p Product) PrimaryAvailableSKU() SKU {
	if len(p.SKUs) == 0 {
		return SKU{}
	}
	// 优先默认规格
	if p.DefaultSKUID != "" {
		for _, sku := range p.SKUs {
			if sku.ID == p.DefaultSKUID && sku.Stock > 0 {
				return sku
			}
		}
	}
	// 其次选有库存的第一个
	for _, sku := range p.SKUs {
		if sku.Stock > 0 {
			return sku
		}
	}
	// 都无库存返回第一个
	return p.SKUs[0]
}

// HasAvailableSKU 报告是否有任何有库存的规格。
func (p Product) HasAvailableSKU() bool {
	for _, sku := range p.SKUs {
		if sku.Stock > 0 {
			return true
		}
	}
	return false
}

// SearchableText 返回用于关键词检索的可搜索文本。
func (p Product) SearchableText() string {
	// 品牌 + 标题 + 品类 + 描述 + 规格名
	parts := []string{p.Brand, p.Title, p.Category, p.Description}
	for _, sku := range p.SKUs {
		parts = append(parts, sku.Spec)
	}
	return joinNonEmpty(parts, " ")
}

// joinNonEmpty 用 sep 连接非空字符串。
func joinNonEmpty(parts []string, sep string) string {
	result := ""
	for _, p := range parts {
		if p != "" {
			if result != "" {
				result += sep
			}
			result += p
		}
	}
	return result
}

// SKU 是商品的一个规格变体。
type SKU struct {
	ID     string // 如 "P1001-S1"
	Spec   string // 如 "军绿色" / "20寸"
	Price  Money  // 规格价格（平台原币种）
	Stock  int    // 库存数量
}

// ToSKUInfo 转换为 SKUInfo 供展示层使用。
func (s SKU) ToSKUInfo() SKUInfo {
	return SKUInfo{
		SKUID:      s.ID,
		Spec:       s.Spec,
		PriceMajor: s.Price.ToMajorUnitsFloat(),
		Currency:   string(s.Price.Currency),
		Stock:      s.Stock,
	}
}

// SKUInfo 是商品卡中展示的规格信息（JSON 序列化用）。
type SKUInfo struct {
	SKUID      string  `json:"sku_id"`
	Spec       string  `json:"spec"`
	PriceMajor float64 `json:"price_major"`
	Currency   string  `json:"currency"`
	Stock      int     `json:"stock"`
}
