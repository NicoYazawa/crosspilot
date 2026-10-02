package persistence

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// CatalogRepo implements ports.ProductRepository using Postgres.
type CatalogRepo struct {
	pool *pgxpool.Pool
}

// NewCatalogRepo constructs a CatalogRepo.
func NewCatalogRepo(pool *pgxpool.Pool) *CatalogRepo {
	return &CatalogRepo{pool: pool}
}

// FindByIDs returns products matching the given IDs, in the same order.
func (r *CatalogRepo) FindByIDs(ctx context.Context, ids []string) ([]catalog.Product, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	const query = `
		SELECT id, title, description, category_id,
		       price_amount_major, price_currency,
		       image_url, rating, review_count, in_stock,
		       attributes, tags,
		       origin_country, brand, material_tags, weight_kg, ships_to,
		       canonical_product_id, source_platform,
		       default_sku_id, source_language, source_locale, data_provenance,
		       updated_at
		FROM catalog.products
		WHERE id = ANY($1)
	`
	rows, err := r.pool.Query(ctx, query, ids)
	if err != nil {
		return nil, fmt.Errorf("catalog_repo: 查询失败: %w", err)
	}
	defer rows.Close()

	result := make([]catalog.Product, 0, len(ids))
	byID := make(map[string]catalog.Product, len(ids))

	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog_repo: 扫描行失败: %w", err)
		}
		byID[p.ID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog_repo: 行迭代失败: %w", err)
	}

	for _, id := range ids {
		if p, ok := byID[id]; ok {
			result = append(result, p)
		}
	}
	return result, nil
}

// ListAll returns all products in the catalog.
func (r *CatalogRepo) ListAll(ctx context.Context) ([]catalog.Product, error) {
	const query = `
		SELECT id, title, description, category_id,
		       price_amount_major, price_currency,
		       image_url, rating, review_count, in_stock,
		       attributes, tags,
		       origin_country, brand, material_tags, weight_kg, ships_to,
		       canonical_product_id, source_platform,
		       default_sku_id, source_language, source_locale, data_provenance,
		       updated_at
		FROM catalog.products
	`
	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("catalog_repo: 查询失败: %w", err)
	}
	defer rows.Close()

	var result []catalog.Product
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog_repo: 扫描行失败: %w", err)
		}
		result = append(result, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog_repo: 行迭代失败: %w", err)
	}
	return result, nil
}

// scanProduct reads a product row.
func scanProduct(row interface {
	Scan(dest ...any) error
}) (catalog.Product, error) {
	var p catalog.Product
	var priceAmt, priceCurr string
	var attrsJSON []byte
	var tags []string
	var rating float64
	var reviewCount int
	var weightKg float64
	var shipsTo []string
	var materialTags []string
	var updatedAt time.Time

	err := row.Scan(
		&p.ID, &p.Title, &p.Description, &p.Category,
		&priceAmt, &priceCurr,
		&p.ImageURL, &rating, &reviewCount, &p.InStock,
		&attrsJSON, &tags,
		&p.OriginCountry, &p.Brand, &materialTags, &weightKg, &shipsTo,
		&p.CanonicalID, &p.SourcePlatform,
		&p.DefaultSKUID, &p.SourceLanguage, &p.SourceLocale, &p.DataProvenance,
		&updatedAt,
	)
	if err != nil {
		return catalog.Product{}, err
	}
	p.UpdatedAt = updatedAt.Format(time.RFC3339)

	p.RatingSummary = map[string]float64{"average": rating, "review_count": float64(reviewCount)}
	p.RatingIsLive = false
	p.WeightKg = weightKg
	p.DimensionsCm = map[string]float64{}
	p.Tags = tags
	p.ShipsTo = shipsTo
	p.MaterialTags = materialTags

	currency, err := catalog.ParseCurrency(priceCurr)
	if err != nil {
		return catalog.Product{}, fmt.Errorf("scan: 解析币种 %q 失败: %w", priceCurr, err)
	}
	money, err := catalog.ParseMoney(priceAmt, currency)
	if err != nil {
		return catalog.Product{}, fmt.Errorf("scan: 解析金额 %q 失败: %w", priceAmt, err)
	}
	p.PrimaryPrice = money

	attrs, err := catalog.AttributesFromJSON(attrsJSON)
	if err != nil {
		return catalog.Product{}, fmt.Errorf("scan: 商品 %s 的属性解析失败: %w", p.ID, err)
	}
	p.Attributes = attrs

	// SKU 明细只存在 attributes.skus 里（表本身是单规格的，见 0003 迁移）。
	// 不解析它，p.SKUs 就恒为空切片——一个「查到了商品但一个规格都没有」的
	// 返回值，调用方拿它去初始化库存只会得到零个 SKU，且看不出哪里不对。
	skus, err := catalog.SKUsFromAttributes(attrs, currency)
	if err != nil {
		return catalog.Product{}, fmt.Errorf("scan: 商品 %s 的规格解析失败: %w", p.ID, err)
	}
	p.SKUs = skus

	return p, nil
}
