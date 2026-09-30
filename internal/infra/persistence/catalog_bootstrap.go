package persistence

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BootstrapLoader 从 catalog-v3.jsonl 加载商品数据到 Postgres。
type BootstrapLoader struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewBootstrapLoader constructs a BootstrapLoader.
func NewBootstrapLoader(pool *pgxpool.Pool, logger *slog.Logger) *BootstrapLoader {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &BootstrapLoader{pool: pool, logger: logger}
}

// jsonProduct 是 catalog-v3.jsonl 中一行的结构。
type jsonProduct struct {
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Description      string            `json:"description"`
	CategoryID       string            `json:"category_id"`
	PriceAmountMajor string            `json:"price_amount_major"`
	PriceCurrency    string            `json:"price_currency"`
	ImageURL         string            `json:"image_url"`
	Rating           float64           `json:"rating"`
	ReviewCount      int               `json:"review_count"`
	InStock          bool              `json:"in_stock"`
	Attributes       map[string]string `json:"attributes"`
	Tags             []string          `json:"tags"`
	OriginCountry    string            `json:"origin_country"`
	Brand            string            `json:"brand"`
	MaterialTags     []string          `json:"material_tags"`
	WeightKg         float64           `json:"weight_kg"`
	ShipsTo          []string          `json:"ships_to"`
	CanonicalProdID  string            `json:"canonical_product_id"`
	SourcePlatform   string            `json:"source_platform"`
	DefaultSkuID     string            `json:"default_sku_id"`
	SourceLanguage   string            `json:"source_language"`
	SourceLocale     string            `json:"source_locale"`
	DataProvenance   string            `json:"data_provenance"`
}

// Load reads catalog-v3.jsonl line by line and upserts each product.
func (l *BootstrapLoader) Load(ctx context.Context, path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("bootstrap: 打开文件失败 %s: %w", path, err)
	}
	defer f.Close()

	const batchSize = 500
	var count int
	var batch []jsonProduct

	scanner := bufio.NewScanner(f)
	// 默认 bufio.Scanner 的 token 上限是 64 KiB，catalog-v3.jsonl 单行可能更大
	const maxTokenSize = 2 * 1024 * 1024 // 2 MiB
	buf := make([]byte, maxTokenSize)
	scanner.Buffer(buf, maxTokenSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var p jsonProduct
		if err := json.Unmarshal(line, &p); err != nil {
			l.logger.WarnContext(ctx, "跳过无效 JSON 行",
				slog.String("error", err.Error()))
			continue
		}

		batch = append(batch, p)
		if len(batch) >= batchSize {
			n, err := l.upsertBatch(ctx, batch)
			if err != nil {
				return count, err
			}
			count += n
			batch = batch[:0]
		}
	}
	if err := scanner.Err(); err != nil {
		return count, fmt.Errorf("bootstrap: 读取文件失败: %w", err)
	}

	// 处理剩余批次
	if len(batch) > 0 {
		n, err := l.upsertBatch(ctx, batch)
		if err != nil {
			return count, err
		}
		count += n
	}

	return count, nil
}

// upsertBatch upserts a batch of products.
func (l *BootstrapLoader) upsertBatch(ctx context.Context, batch []jsonProduct) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 先把 attributes 序列化为 JSON
	type row struct {
		jsonProduct
		AttributesJSON []byte
	}
	rows := make([]row, len(batch))
	for i := range batch {
		attrsJSON, err := json.Marshal(batch[i].Attributes)
		if err != nil {
			return 0, fmt.Errorf("bootstrap: 序列化属性失败: %w", err)
		}
		rows[i] = row{jsonProduct: batch[i], AttributesJSON: attrsJSON}
	}

	const query = `
		INSERT INTO catalog.products (
			id, title, description, category_id,
			price_amount_major, price_currency,
			image_url, rating, review_count, in_stock,
			attributes, tags,
			origin_country, brand, material_tags, weight_kg, ships_to,
			canonical_product_id, source_platform,
			default_sku_id, source_language, source_locale, data_provenance,
			created_at, updated_at
		) VALUES (
			$1, $2, $3, $4,
			$5, $6,
			$7, $8, $9, $10,
			$11, $12,
			$13, $14, $15, $16, $17,
			$18, $19,
			$20, $21, $22, $23,
			now(), now()
		)
		ON CONFLICT (id) DO UPDATE SET
			title           = EXCLUDED.title,
			description     = EXCLUDED.description,
			category_id     = EXCLUDED.category_id,
			price_amount_major = EXCLUDED.price_amount_major,
			price_currency  = EXCLUDED.price_currency,
			image_url       = EXCLUDED.image_url,
			rating          = EXCLUDED.rating,
			review_count    = EXCLUDED.review_count,
			in_stock        = EXCLUDED.in_stock,
			attributes      = EXCLUDED.attributes,
			tags            = EXCLUDED.tags,
			origin_country  = EXCLUDED.origin_country,
			brand           = EXCLUDED.brand,
			material_tags   = EXCLUDED.material_tags,
			weight_kg       = EXCLUDED.weight_kg,
			ships_to        = EXCLUDED.ships_to,
			canonical_product_id = EXCLUDED.canonical_product_id,
			source_platform = EXCLUDED.source_platform,
			default_sku_id  = EXCLUDED.default_sku_id,
			source_language = EXCLUDED.source_language,
			source_locale   = EXCLUDED.source_locale,
			data_provenance = EXCLUDED.data_provenance,
			updated_at      = now()
	`

	batchLen := len(rows)
	_, err := l.pool.CopyFrom(
		ctx,
		pgx.Identifier{"catalog", "products"},
		[]string{
			"id", "title", "description", "category_id",
			"price_amount_major", "price_currency",
			"image_url", "rating", "review_count", "in_stock",
			"attributes", "tags",
			"origin_country", "brand", "material_tags", "weight_kg", "ships_to",
			"canonical_product_id", "source_platform",
			"default_sku_id", "source_language", "source_locale", "data_provenance",
		},
		pgx.CopyFromSlice(batchLen, func(i int) ([]any, error) {
			r := rows[i]
			return []any{
				r.ID, r.Title, r.Description, r.CategoryID,
				r.PriceAmountMajor, r.PriceCurrency,
				r.ImageURL, r.Rating, r.ReviewCount, r.InStock,
				r.AttributesJSON, r.Tags,
				r.OriginCountry, r.Brand, r.MaterialTags, r.WeightKg, r.ShipsTo,
				r.CanonicalProdID, r.SourcePlatform,
				r.DefaultSkuID, r.SourceLanguage, r.SourceLocale, r.DataProvenance,
			}, nil
		}),
	)
	if err != nil {
		// CopyFrom 失败时退回到逐条插入
		return l.upsertBatchRowByRow(ctx, batch)
	}
	return batchLen, nil
}

// upsertBatchRowByRow 是 CopyFrom 失败时的回退，用逐条 INSERT ... ON CONFLICT 实现。
func (l *BootstrapLoader) upsertBatchRowByRow(ctx context.Context, batch []jsonProduct) (int, error) {
	const query = `
		INSERT INTO catalog.products (
			id, title, description, category_id,
			price_amount_major, price_currency,
			image_url, rating, review_count, in_stock,
			attributes, tags,
			origin_country, brand, material_tags, weight_kg, ships_to,
			canonical_product_id, source_platform,
			default_sku_id, source_language, source_locale, data_provenance
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		ON CONFLICT (id) DO UPDATE SET
			title           = EXCLUDED.title,
			description     = EXCLUDED.description,
			category_id     = EXCLUDED.category_id,
			price_amount_major = EXCLUDED.price_amount_major,
			price_currency  = EXCLUDED.price_currency,
			image_url       = EXCLUDED.image_url,
			rating          = EXCLUDED.rating,
			review_count    = EXCLUDED.review_count,
			in_stock        = EXCLUDED.in_stock,
			attributes      = EXCLUDED.attributes,
			tags            = EXCLUDED.tags,
			origin_country  = EXCLUDED.origin_country,
			brand           = EXCLUDED.brand,
			material_tags   = EXCLUDED.material_tags,
			weight_kg       = EXCLUDED.weight_kg,
			ships_to        = EXCLUDED.ships_to,
			canonical_product_id = EXCLUDED.canonical_product_id,
			source_platform = EXCLUDED.source_platform,
			default_sku_id  = EXCLUDED.default_sku_id,
			source_language = EXCLUDED.source_language,
			source_locale   = EXCLUDED.source_locale,
			data_provenance = EXCLUDED.data_provenance,
			updated_at      = now()
	`
	count := 0
	for _, p := range batch {
		attrsJSON, err := json.Marshal(p.Attributes)
		if err != nil {
			return count, fmt.Errorf("bootstrap: 序列化属性失败: %w", err)
		}
		_, err = l.pool.Exec(ctx, query,
			p.ID, p.Title, p.Description, p.CategoryID,
			p.PriceAmountMajor, p.PriceCurrency,
			p.ImageURL, p.Rating, p.ReviewCount, p.InStock,
			attrsJSON, p.Tags,
			p.OriginCountry, p.Brand, p.MaterialTags, p.WeightKg, p.ShipsTo,
			p.CanonicalProdID, p.SourcePlatform,
			p.DefaultSkuID, p.SourceLanguage, p.SourceLocale, p.DataProvenance,
		)
		if err != nil {
			return count, fmt.Errorf("bootstrap: 插入商品 %s 失败: %w", p.ID, err)
		}
		count++
	}
	return count, nil
}
