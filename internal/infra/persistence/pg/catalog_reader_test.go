package pg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg/pgtest"
)

// catalogMigratedPool 建一个跑完全部迁移的独立库，供商品目录读适配器的用例复用。
func catalogMigratedPool(t dbHelper) *pgxpool.Pool {
	t.Helper()
	return pgtest.NewDatabase(t, func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pg.Migrate(ctx, pool, pg.Up)
		return err
	})
}

// seedProducts 灌入商品行。SQL 直接写死而不是经导入器：本用例要钉的是**读取端**
// 对库中既有形状的解释，走导入器会把「导入器怎么写」也拖进断言范围。
func seedProducts(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool := catalogMigratedPool(t)
	ctx := context.Background()

	stmts := []string{
		// 两个规格、价格不同：读取端必须两个都给出，且各自带自己的价。
		`INSERT INTO catalog.products
		     (id, title, category_id, price_amount_major, price_currency, default_sku_id, in_stock, attributes)
		 VALUES ('P1001', 'Nomadica 旅行三件套', 'travel', '189.0', 'CNY', 'P1001-S1', true,
		     '{"skus":[
		        {"sku_id":"P1001-S1","spec":"军绿色","currency":"CNY","price_major":189.0,"stock":50},
		        {"sku_id":"P1001-S2","spec":"沙漠黄","currency":"CNY","price_major":199.0,"stock":30}
		      ]}'::jsonb)`,
		// 老数据：没有 attributes，只有权威的 default_sku_id + price_amount_major。
		`INSERT INTO catalog.products
		     (id, title, category_id, price_amount_major, price_currency, default_sku_id, in_stock, attributes)
		 VALUES ('P2001', '无规格明细的老商品', 'other', '12.50', 'USD', 'P2001-S1', true, NULL)`,
		// default_sku_id 为空：必须退回商品 ID，不能留下空 SKU 标识。
		`INSERT INTO catalog.products
		     (id, title, category_id, price_amount_major, price_currency, default_sku_id, in_stock, attributes)
		 VALUES ('P3001', '没有规格标识', 'other', '9.99', 'USD', NULL, true, NULL)`,
		// 日元零小数位：2980 不能变成 29.80 或 298000。
		`INSERT INTO catalog.products
		     (id, title, category_id, price_amount_major, price_currency, default_sku_id, in_stock, attributes)
		 VALUES ('P4001', '日淘好物', 'other', '2980', 'JPY', 'P4001-S1', true,
		     '{"skus":[{"sku_id":"P4001-S1","spec":"标准","currency":"JPY","price_major":2980,"stock":7}]}'::jsonb)`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("灌种子失败：%v\nSQL: %s", err, stmt)
		}
	}
	return pool
}

// TestPgCatalogFind_返回全部规格 断言下单能拿到商品下的每一个规格。
//
// 这是回归用例：Find 曾经只返回硬编码的默认规格，一件商品的两个规格里只有
// 第一个能下单，第二个被判成「规格不存在」——目录里明明有，下单却查无此规格。
func TestPgCatalogFind_返回全部规格(t *testing.T) {
	pool := seedProducts(t)

	got, found, err := pg.NewCatalog(pool).Find(context.Background(), "P1001")
	if err != nil {
		t.Fatalf("Find 失败：%v", err)
	}
	if !found {
		t.Fatal("P1001 存在，found 却为 false")
	}
	if got.ProductID != "P1001" || got.Title != "Nomadica 旅行三件套" {
		t.Errorf("商品标识 = (%q, %q)", got.ProductID, got.Title)
	}
	if len(got.SKUs) != 2 {
		t.Fatalf("规格数 = %d，期望 2", len(got.SKUs))
	}

	s1, s2 := got.SKUs[0], got.SKUs[1]
	if s1.SKUID != "P1001-S1" || s1.Spec != "军绿色" {
		t.Errorf("第一个规格 = (%q, %q)", s1.SKUID, s1.Spec)
	}
	if s1.Price.String() != "189.00 CNY" {
		t.Errorf("第一个规格价格 = %q，期望 189.00 CNY", s1.Price.String())
	}
	if s2.SKUID != "P1001-S2" || s2.Spec != "沙漠黄" {
		t.Errorf("第二个规格 = (%q, %q)", s2.SKUID, s2.Spec)
	}
	// 两个规格价格不同：第二个规格不能被主价格盖掉，否则下单金额就是错的。
	if s2.Price.String() != "199.00 CNY" {
		t.Errorf("第二个规格价格 = %q，期望 199.00 CNY（不能被商品主价格盖掉）", s2.Price.String())
	}
	if s2.Stock != 30 {
		t.Errorf("第二个规格库存 = %d，期望 30", s2.Stock)
	}
}

// TestPgCatalogFind_属性缺席时退回单规格 断言老数据仍可下单。
func TestPgCatalogFind_属性缺席时退回单规格(t *testing.T) {
	pool := seedProducts(t)

	got, found, err := pg.NewCatalog(pool).Find(context.Background(), "P2001")
	if err != nil {
		t.Fatalf("Find 失败：%v", err)
	}
	if !found {
		t.Fatal("P2001 存在，found 却为 false")
	}
	if len(got.SKUs) != 1 {
		t.Fatalf("规格数 = %d，期望 1（没有 attributes 时退回库里的默认规格）", len(got.SKUs))
	}
	if got.SKUs[0].SKUID != "P2001-S1" {
		t.Errorf("SKUID = %q，期望 P2001-S1", got.SKUs[0].SKUID)
	}
	if got.SKUs[0].Price.String() != "12.50 USD" {
		t.Errorf("价格 = %q，期望 12.50 USD", got.SKUs[0].Price.String())
	}
}

// TestPgCatalogFind_规格标识为空时退回商品ID 断言确认单的每一行都有可落的规格位。
func TestPgCatalogFind_规格标识为空时退回商品ID(t *testing.T) {
	pool := seedProducts(t)

	got, _, err := pg.NewCatalog(pool).Find(context.Background(), "P3001")
	if err != nil {
		t.Fatalf("Find 失败：%v", err)
	}
	if len(got.SKUs) != 1 {
		t.Fatalf("规格数 = %d，期望 1", len(got.SKUs))
	}
	if got.SKUs[0].SKUID != "P3001" {
		t.Errorf("SKUID = %q，期望退回商品 ID P3001（空 SKU 标识无处落库存记录）", got.SKUs[0].SKUID)
	}
}

// TestPgCatalogFind_零小数位币种保真 断言金额是解析出来的，不是按两位小数切出来的。
func TestPgCatalogFind_零小数位币种保真(t *testing.T) {
	pool := seedProducts(t)

	got, _, err := pg.NewCatalog(pool).Find(context.Background(), "P4001")
	if err != nil {
		t.Fatalf("Find 失败：%v", err)
	}
	if len(got.SKUs) != 1 {
		t.Fatalf("规格数 = %d，期望 1", len(got.SKUs))
	}
	if got.SKUs[0].Price.Currency != catalog.JPY {
		t.Errorf("币种 = %q，期望 JPY", got.SKUs[0].Price.Currency)
	}
	if got.SKUs[0].Price.String() != "2980 JPY" {
		t.Errorf("价格 = %q，期望 2980 JPY", got.SKUs[0].Price.String())
	}
}

// TestPgCatalogFind_查无此货与查询出错分开 断言「目录里没有」不是故障。
func TestPgCatalogFind_查无此货与查询出错分开(t *testing.T) {
	pool := seedProducts(t)

	_, found, err := pg.NewCatalog(pool).Find(context.Background(), "不存在的商品")
	if err != nil {
		t.Fatalf("商品不存在不应报错（上层要据此回 404），实际：%v", err)
	}
	if found {
		t.Error("商品不存在时 found 应为 false")
	}
}
