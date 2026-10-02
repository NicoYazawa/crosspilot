package persistence

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg/pgtest"
)

func TestCatalogRepo_FindByIDs(t *testing.T) {
	pool := pgtest.NewDatabase(t, migrateCatalog)
	t.Cleanup(func() { pool.Close() })

	ctx := context.Background()
	repo := NewCatalogRepo(pool)

	insertProducts(ctx, t, pool, []catalog.Product{
		{
			ID:           "SKU-001",
			Title:        "测试商品 A",
			Description:  "描述 A",
			Category:     "cat-1",
			PrimaryPrice: catalog.MustMoney("29.99", catalog.USD),
			ImageURL:     "https://example.com/a.jpg",
			InStock:      true,
			Attributes:   map[string]any{"color": "red", "size": "M"},
			Tags:         []string{"tag-a", "tag-b"},
			ShipsTo:      []string{"CN", "US"},
		},
		{
			ID:           "SKU-002",
			Title:        "测试商品 B",
			Description:  "描述 B",
			Category:     "cat-2",
			PrimaryPrice: catalog.MustMoney("99.00", catalog.EUR),
			ImageURL:     "https://example.com/b.jpg",
			InStock:      false,
			Attributes:   map[string]any{"color": "blue"},
			Tags:         []string{"tag-b"},
			ShipsTo:      []string{"CN"},
		},
		{
			ID:           "SKU-003",
			Title:        "测试商品 C",
			Description:  "描述 C",
			Category:     "cat-1",
			PrimaryPrice: catalog.MustMoney("15.50", catalog.USD),
			ImageURL:     "https://example.com/c.jpg",
			InStock:      true,
			Attributes:   map[string]any{"color": "green"},
			Tags:         []string{"tag-c"},
			ShipsTo:      []string{"CN", "US", "EU"},
		},
	})

	t.Run("FindByIDs 返回正确商品并保持顺序", func(t *testing.T) {
		ids := []string{"SKU-003", "SKU-001"}
		products, err := repo.FindByIDs(ctx, ids)
		if err != nil {
			t.Fatalf("FindByIDs 失败: %v", err)
		}
		if len(products) != 2 {
			t.Fatalf("期望 2 件商品，实际 %d 件", len(products))
		}
		if products[0].ID != "SKU-003" {
			t.Errorf("第一件应为 SKU-003，实际为 %s", products[0].ID)
		}
		if products[1].ID != "SKU-001" {
			t.Errorf("第二件应为 SKU-001，实际为 %s", products[1].ID)
		}
		if products[0].PrimaryPrice.Amount.String() != "15.50" {
			t.Errorf("SKU-003 价格应为 15.50，实际 %s", products[0].PrimaryPrice.Amount.String())
		}
		if products[1].PrimaryPrice.Amount.String() != "29.99" {
			t.Errorf("SKU-001 价格应为 29.99，实际 %s", products[1].PrimaryPrice.Amount.String())
		}
	})

	t.Run("FindByIDs 忽略不存在的 ID", func(t *testing.T) {
		ids := []string{"SKU-001", "NOT-EXIST", "SKU-002"}
		products, err := repo.FindByIDs(ctx, ids)
		if err != nil {
			t.Fatalf("FindByIDs 失败: %v", err)
		}
		if len(products) != 2 {
			t.Fatalf("期望 2 件商品（忽略不存在的），实际 %d 件", len(products))
		}
	})

	t.Run("FindByIDs 空列表返回空", func(t *testing.T) {
		products, err := repo.FindByIDs(ctx, nil)
		if err != nil {
			t.Fatalf("FindByIDs 失败: %v", err)
		}
		if len(products) != 0 {
			t.Fatalf("期望空列表，实际 %d 件", len(products))
		}
	})
}

func TestCatalogRepo_ListAll(t *testing.T) {
	pool := pgtest.NewDatabase(t, migrateCatalog)
	t.Cleanup(func() { pool.Close() })

	ctx := context.Background()
	repo := NewCatalogRepo(pool)

	insertProducts(ctx, t, pool, []catalog.Product{
		{
			ID:           "LIST-001",
			Title:        "列表商品 1",
			Description:  "描述",
			Category:     "cat-x",
			PrimaryPrice: catalog.MustMoney("10.00", catalog.USD),
			InStock:      true,
			Attributes:   map[string]any{},
			Tags:         []string{},
			ShipsTo:      []string{"CN"},
		},
		{
			ID:           "LIST-002",
			Title:        "列表商品 2",
			Description:  "描述",
			Category:     "cat-y",
			PrimaryPrice: catalog.MustMoney("20.00", catalog.USD),
			InStock:      true,
			Attributes:   map[string]any{},
			Tags:         []string{},
			ShipsTo:      []string{"CN"},
		},
		{
			ID:           "LIST-003",
			Title:        "列表商品 3",
			Description:  "描述",
			Category:     "cat-z",
			PrimaryPrice: catalog.MustMoney("30.00", catalog.USD),
			InStock:      false,
			Attributes:   map[string]any{},
			Tags:         []string{},
			ShipsTo:      []string{"CN"},
		},
	})

	t.Run("ListAll 返回全部商品", func(t *testing.T) {
		products, err := repo.ListAll(ctx)
		if err != nil {
			t.Fatalf("ListAll 失败: %v", err)
		}
		if len(products) != 3 {
			t.Fatalf("期望 3 件商品，实际 %d 件", len(products))
		}
	})

	t.Run("ListAll 返回非 nil 切片", func(t *testing.T) {
		// 共享 pool 有前面插入的数据，验证 ListAll 返回非 nil 切片
		emptyRepo := NewCatalogRepo(pool)
		products, err := emptyRepo.ListAll(ctx)
		if err != nil {
			t.Fatalf("ListAll 失败: %v", err)
		}
		if products == nil {
			t.Error("期望非 nil 切片")
		}
	})
}

// migrateCatalog 对测试库执行 catalog.products 迁移。
func migrateCatalog(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS catalog;
		CREATE TABLE IF NOT EXISTS catalog.products (
			id TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			description TEXT,
			category_id TEXT NOT NULL,
			price_amount_major TEXT NOT NULL,
			price_currency TEXT NOT NULL,
			image_url TEXT,
			rating REAL,
			review_count INTEGER,
			in_stock BOOLEAN DEFAULT true,
			attributes JSONB,
			tags TEXT[],
			origin_country TEXT DEFAULT '',
			brand TEXT DEFAULT '',
			material_tags TEXT[],
			weight_kg REAL DEFAULT 0,
			ships_to TEXT[],
			canonical_product_id TEXT,
			source_platform TEXT DEFAULT '',
			updated_at TIMESTAMPTZ DEFAULT now(),
			default_sku_id TEXT,
			source_language TEXT DEFAULT '',
			source_locale TEXT DEFAULT '',
			data_provenance TEXT DEFAULT '',
			created_at TIMESTAMPTZ DEFAULT now()
		);
		CREATE INDEX IF NOT EXISTS idx_products_category ON catalog.products(category_id);
		CREATE INDEX IF NOT EXISTS idx_products_in_stock ON catalog.products(in_stock);
	`)
	return err
}

// insertProducts 批量插入测试商品。
//
// ctx 置于 t 之前是为了满足 context-as-argument（ctx 必须是首参）；调用方各自
// 已持有同一个 ctx，因此不复用 t.Context()，避免出现两个上下文来源。
func insertProducts(ctx context.Context, t *testing.T, pool *pgxpool.Pool, products []catalog.Product) {
	t.Helper()
	// 按下标取址：catalog.Product 约 450 字节，逐元素值拷贝纯属浪费，且此处只读。
	for i := range products {
		prod := &products[i]
		_, err := pool.Exec(ctx, `
			INSERT INTO catalog.products (
				id, title, description, category_id,
				price_amount_major, price_currency,
				image_url, rating, review_count, in_stock,
				attributes, tags, origin_country, brand, material_tags,
				weight_kg, ships_to, canonical_product_id, source_platform,
				default_sku_id, source_language, source_locale, data_provenance
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		`,
			prod.ID,
			prod.Title,
			prod.Description,
			prod.Category,
			prod.PrimaryPrice.Amount.String(),
			string(prod.PrimaryPrice.Currency),
			prod.ImageURL,
			0.0, // rating
			0,   // review_count
			prod.InStock,
			nil, // attributes JSON
			prod.Tags,
			prod.OriginCountry,
			prod.Brand,
			prod.MaterialTags,
			prod.WeightKg,
			prod.ShipsTo,
			prod.CanonicalID,
			prod.SourcePlatform,
			prod.DefaultSKUID,
			prod.SourceLanguage,
			prod.SourceLocale,
			prod.DataProvenance,
		)
		if err != nil {
			t.Fatalf("插入商品 %s 失败: %v", prod.ID, err)
		}
	}
}
