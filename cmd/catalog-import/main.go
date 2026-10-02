// Command catalog-import 把商品目录导入到**可下单状态**：先写 catalog.products，
// 再用目录里的规格初始化账本库存（trade_sku_inventory）。
//
// 为什么是独立命令而不是 server 启动时顺手导入：导入需要真实数据库连接，而
// container.Build 的约定是「装配不建立真实连接」（见 container.go 的 Build
// 注释）——把一次 3,700 行的写库操作塞进装配阶段会破坏这条约定，也会让 server
// 的启动时间取决于一份外部文件的大小。做成独立命令后，它和 migrate 一样是
// 一个有明确输入输出的生命周期步骤，能单独重跑、单独排障。
//
// 为什么要连库存一起做：只写 catalog.products 的商品是**下不了单**的。下单要
// 先过库存校验（见 domain/trade/prepare.go 的 quoteOf），库存表为空时每一件
// 商品都会以 NOT_FOUND「商品 SKU 不存在或尚未初始化持久库存」被拒。两步合在
// 一条命令里，「导入完成」与「能下单」才是同一件事。
//
// 用法：
//
//	catalog-import                  # 读 CATALOG_BOOTSTRAP_PATH，商品库非空则跳过写入
//	catalog-import -force           # 即使商品库非空也重写（upsert 幂等）
//	catalog-import -file 某路径     # 指定文件，优先于环境变量
//	catalog-import -skip-inventory  # 只导商品，不碰账本库存
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	tradesvc "github.com/NicoYazawa/crosspilot/internal/application/trade"
	"github.com/NicoYazawa/crosspilot/internal/config"
	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/trade"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
	"github.com/NicoYazawa/crosspilot/internal/infra/postgres"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "crosspilot-catalog-import: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("catalog-import", flag.ContinueOnError)
	file := fs.String("file", "", "商品引导文件路径（默认取 CATALOG_BOOTSTRAP_PATH）")
	force := fs.Bool("force", false, "库中已有商品时仍然导入（upsert 幂等）")
	skipInventory := fs.Bool("skip-inventory", false, "只导商品，不初始化账本库存")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	path := *file
	if path == "" {
		path = cfg.Catalog.BootstrapPath
	}
	if path == "" {
		return fmt.Errorf("未指定引导文件：用 -file 或设置 CATALOG_BOOTSTRAP_PATH")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := postgres.Check(ctx, pool); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	clock := systemClock{}

	if err := importProducts(ctx, logger, pool, path, *force); err != nil {
		return err
	}

	if *skipInventory {
		fmt.Println("已按 -skip-inventory 跳过账本库存初始化")
		return nil
	}
	return seedInventory(ctx, logger, pool, clock)
}

// importProducts 把引导文件写进 catalog.products。
func importProducts(
	ctx context.Context,
	logger *slog.Logger,
	pool *pgxpool.Pool,
	path string,
	force bool,
) error {
	loader := persistence.NewBootstrapLoader(pool, logger)

	if !force {
		existing, err := loader.Count(ctx)
		if err != nil {
			return err
		}
		if existing > 0 {
			fmt.Printf("catalog.products 已有 %d 件商品，跳过导入（要强制重导加 -force）\n", existing)
			return nil
		}
	}

	n, err := loader.Load(ctx, path)
	if err != nil {
		return err
	}

	fmt.Printf("从 %s 导入 %d 件商品\n", path, n)
	return nil
}

// seedInventory 用目录里已落库的规格初始化账本库存。
//
// 数据源刻意选「读库」而不是「读文件」：文件只在这一步之前可能已经不在了
// （-force 重导、或者商品早已导入只补库存），库是唯一的事实源。副作用是
// CatalogRepo 终于有了调用方——它此前实现完整、测试齐全，却没有任何生产代码
// 用过，`FindByIDs` / `ListAll` 里的问题因此一直没机会暴露。
func seedInventory(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, clock pg.Clock) error {
	repo := persistence.NewCatalogRepo(pool)

	products, err := repo.ListAll(ctx)
	if err != nil {
		return fmt.Errorf("读取商品目录失败: %w", err)
	}
	if len(products) == 0 {
		return fmt.Errorf("商品目录为空，没有可初始化库存的规格：先导入商品（去掉 -skip-inventory）")
	}

	seeds, skipped, err := buildSeedSKUs(products)
	if err != nil {
		return err
	}
	if skipped > 0 {
		// 不静默：这些商品的库存没有落库，下单时会以「SKU 不存在」被拒。
		logger.WarnContext(ctx, "部分商品没有规格明细，未初始化库存",
			slog.Int("products", skipped))
	}

	store, err := pg.New(pg.Config{Pool: pool, Clock: clock, Logger: logger})
	if err != nil {
		return err
	}
	svc, err := tradesvc.New(tradesvc.Config{Store: store, Catalog: pg.NewCatalog(pool), Clock: clock})
	if err != nil {
		return err
	}

	if err := svc.InitializeInventory(ctx, seeds); err != nil {
		return fmt.Errorf("初始化账本库存失败: %w", err)
	}

	fmt.Printf("初始化库存 %d 个规格（覆盖 %d 件商品，跳过无规格商品 %d 件）\n",
		len(seeds), len(products)-skipped, skipped)
	return nil
}

// buildSeedSKUs 把目录商品展开成「一行一个规格」的库存种子。
//
// 返回 skipped 是**没有规格明细**的商品数。它们不生成种子而不是退回「用一个
// 主价格补一条」：库存种子里 stock 是必填的，凭空给一个数字就是编造可售数量，
// 而这种编造在下单时会变成真实的超卖。
func buildSeedSKUs(products []catalog.Product) (seeds []trade.SeedSKU, skipped int, err error) {
	seeds = make([]trade.SeedSKU, 0, len(products))

	for i := range products {
		p := &products[i]
		if len(p.SKUs) == 0 {
			skipped++
			continue
		}
		for _, sku := range p.SKUs {
			// 账本只认最小货币单位，换算只在这一处发生（与下单路径同一函数）。
			minor, err := trade.MinorUnits(sku.Price)
			if err != nil {
				return nil, 0, fmt.Errorf("商品 %s 的规格 %s 价格无法换算: %w", p.ID, sku.ID, err)
			}
			seeds = append(seeds, trade.SeedSKU{
				SKUID:          sku.ID,
				ProductID:      p.ID,
				Title:          skuTitle(p.Title, sku.Spec),
				Stock:          int64(sku.Stock),
				UnitPriceMinor: minor,
				Currency:       sku.Price.Currency,
			})
		}
	}
	return seeds, skipped, nil
}

// skuTitle 拼出规格级的展示标题，与下单路径（application/trade 的 resolveItems）
// 的拼法保持一致：同一条规格在库存表和确认单上应当是同一个名字。
func skuTitle(productTitle, spec string) string {
	if spec == "" {
		return productTitle
	}
	return fmt.Sprintf("%s（%s）", productTitle, spec)
}

// systemClock 是两个下层接口（pg.Clock 与 tradesvc.Clock，形状相同）共用的时钟。
// 各自包一个适配器只会让「现在几点」出现多个来源。
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }
