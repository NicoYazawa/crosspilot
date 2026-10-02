// Package persistence 提供目录数据的 Postgres 持久化适配器与批量引导加载。
package persistence

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// jsonProduct 是 data/catalog-v3.jsonl 中一行的结构，即源项目 globex-agent
// 的 SPU 形态。
//
// 字段名与顺序都对齐**文件里真实的键**，不是「我们希望它长什么样」。此前这套
// 结构体是按一个想象出来的扁平 schema 写的（`id` / `price_amount_major` /
// `rating` 各自一个键），与源文件对不上：源里价格在 `skus[]` 内部、评分在
// `rating_summary` 里、ID 叫 `product_id`。结果是即便拿到文件，唯一非零的字段
// 只有 title 与 description——导入「成功」但商品没有价格，下单路径随即失效。
type jsonProduct struct {
	ProductID          string          `json:"product_id"`
	Title              string          `json:"title"`
	Description        string          `json:"description"`
	Category           string          `json:"category"`
	Brand              string          `json:"brand"`
	OriginCountry      string          `json:"origin_country"`
	MaterialTags       []string        `json:"material_tags"`
	WeightKg           float64         `json:"weight_kg"`
	ShipsTo            []string        `json:"ships_to"`
	SKUs               []jsonSKU       `json:"skus"`
	RatingSummary      jsonRating      `json:"rating_summary"`
	Highlights         []jsonHighlight `json:"highlights"`
	DimensionsCM       map[string]any  `json:"dimensions_cm"`
	TaxCategory        string          `json:"tax_category"`
	ExternalProductID  string          `json:"external_product_id"`
	CanonicalProductID string          `json:"canonical_product_id"`
	SourcePlatform     string          `json:"source_platform"`
	SourceLanguage     string          `json:"source_language"`
	SourceLocale       string          `json:"source_locale"`
	DataProvenance     string          `json:"data_provenance"`
	UpdatedAt          string          `json:"updated_at"`
}

// jsonSKU 是商品下的一个规格。
//
// PriceMajor 用 json.Number 而非 float64：金额从 JSON 一路走到 catalog.products
// 的 price_amount_major（TEXT，十进制字符串）再交给领域层的 decimal 解析，
// 中间经手 float64 就会引入二进制浮点误差。json.Number 保留的是**字面量原文**
// （源文件里 `189.0` 与 `189` 两种写法都有），导库后与文件逐字一致。
type jsonSKU struct {
	SKUID      string      `json:"sku_id"`
	Spec       string      `json:"spec"`
	Currency   string      `json:"currency"`
	PriceMajor json.Number `json:"price_major"`
	Stock      int         `json:"stock"`
}

// jsonRating 是源文件里的评分块。
type jsonRating struct {
	Average     float64 `json:"average"`
	ReviewCount int     `json:"review_count"`
}

// jsonHighlight 是商品卖点。
type jsonHighlight struct {
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// productRow 是 catalog.products 的一行，即落库形态。
//
// 与 jsonProduct 分开是有意的：文件是外部事实，表是本服务的存储契约，两者
// 的字段名与基数都不同（一个商品在文件里带 N 个规格，在表里只有一个价格）。
// 把映射收敛在 toRow 一处，比让 upsert 语句去迁就外部字段名更好审——「哪来的
// 数据、丢了什么」在一个函数里看完。
type productRow struct {
	ID               string
	Title            string
	Description      string
	CategoryID       string
	PriceAmountMajor string
	PriceCurrency    string
	ImageURL         string
	Rating           float64
	ReviewCount      int
	InStock          bool
	Attributes       map[string]any
	Tags             []string
	OriginCountry    string
	Brand            string
	MaterialTags     []string
	WeightKg         float64
	ShipsTo          []string
	CanonicalProdID  string
	SourcePlatform   string
	DefaultSkuID     string
	SourceLanguage   string
	SourceLocale     string
	DataProvenance   string
}

// errNoSKU 表示一行商品没有任何规格，无法确定价格与默认规格。
var errNoSKU = errors.New("商品没有任何规格")

// toRow 把文件里的一行映射成 catalog.products 的一行。
//
// 映射中**有损**的地方（表是单规格的，文件 92% 的商品有两个规格）一律不靠
// 编造补齐：取首个规格作默认规格与报价，完整 skus 数组原样存进 attributes，
// 谁要读第二个规格的价格也能读得到，只是不在商品行的列上。表结构从单规格
// 升级到独立 SKU 表之后，这段映射即可退化为「逐规格展开」。
func (p jsonProduct) toRow() (productRow, error) {
	if p.ProductID == "" {
		return productRow{}, errors.New("商品缺少 product_id")
	}
	if len(p.SKUs) == 0 {
		return productRow{}, errNoSKU
	}

	// 默认规格 = 首个规格，报价与 default_sku_id 取自**同一个** SKU。
	// 若价格取最低、SKU 取第一个，两者会指向不同规格，下单时价格与规格对不上。
	def := p.SKUs[0]
	if def.PriceMajor.String() == "" {
		return productRow{}, fmt.Errorf("默认规格 %s 缺少 price_major", def.SKUID)
	}

	// in_stock 取全部规格的并集：只要有任一规格有货，商品就算在售。
	// 源文件有 370 件商品全部规格库存为 0，直接写 true 会把它们当有货商品返回。
	inStock := false
	for _, s := range p.SKUs {
		if s.Stock > 0 {
			inStock = true
			break
		}
	}

	attrs := map[string]any{}
	// skus 全量入 attributes，是上面「有损映射」的补偿，也是保真依据：
	// 想核对某个规格的价格，不必回到源文件。
	attrs["skus"] = p.SKUs
	if len(p.Highlights) > 0 {
		attrs["highlights"] = p.Highlights
	}
	if len(p.DimensionsCM) > 0 {
		attrs["dimensions_cm"] = p.DimensionsCM
	}
	if p.TaxCategory != "" {
		attrs["tax_category"] = p.TaxCategory
	}
	if p.ExternalProductID != "" {
		attrs["external_product_id"] = p.ExternalProductID
	}
	if p.UpdatedAt != "" {
		attrs["source_updated_at"] = p.UpdatedAt
	}

	return productRow{
		ID:               p.ProductID,
		Title:            p.Title,
		Description:      p.Description,
		CategoryID:       p.Category,
		PriceAmountMajor: def.PriceMajor.String(),
		PriceCurrency:    def.Currency,
		// 源文件没有图片字段；留空而不是填占位图 URL——前端据此显示无图，
		// 填一个假 URL 会变成前端拿不到的坏图。
		ImageURL:        "",
		Rating:          p.RatingSummary.Average,
		ReviewCount:     p.RatingSummary.ReviewCount,
		InStock:         inStock,
		Attributes:      attrs,
		Tags:            nil, // 源文件无独立 tags 键（evaluation_tags 是评测元数据，不是商品标签）
		OriginCountry:   p.OriginCountry,
		Brand:           p.Brand,
		MaterialTags:    p.MaterialTags,
		WeightKg:        p.WeightKg,
		ShipsTo:         p.ShipsTo,
		CanonicalProdID: p.CanonicalProductID,
		SourcePlatform:  p.SourcePlatform,
		DefaultSkuID:    def.SKUID,
		SourceLanguage:  p.SourceLanguage,
		SourceLocale:    p.SourceLocale,
		DataProvenance:  p.DataProvenance,
	}, nil
}

// openCatalogFile 打开引导文件，按魔数自动识别 gzip。
//
// 仓库里存的是 catalog-v3.jsonl.gz（4.6 MB → 0.31 MB，15 倍），但解压后的
// 明文同样要能直接喂进来（运维手头往往是明文，图省事不该被格式卡住）。
// 因此不设开关，读前两个字节判断：这是 gzip 就套一层解压，否则按原样读。
func openCatalogFile(path string) (io.ReadCloser, error) {
	// #nosec G304 -- 引导文件路径由运维通过启动参数指定，不是用户可注入的输入
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: 打开文件失败 %s: %w", path, err)
	}

	br := bufio.NewReader(f)
	// Peek 取前两个字节判断魔数。读不满 2 字节（空文件或只有 1 字节）不是打开
	// 失败，而是「这个文件没有数据」，与明文分支合流交给扫描器处理；因此这里
	// 不需要区分 Peek 的错误与「不是 gzip」——两种情况的下一步动作完全相同。
	//
	// 这里必须返回 br 而不是 f：Peek 已经把文件头部若干字节读进了 br 的
	// 缓冲区，直接还回去等于把这段数据丢掉——表现是文件开头凭空少一截，
	// 首行被截断、后面整段对不上，而错误现场看起来像是「文件本身有问题」。
	magic, _ := br.Peek(2)
	if len(magic) < 2 || magic[0] != 0x1f || magic[1] != 0x8b {
		return &fileReader{Reader: br, closers: []io.Closer{f}}, nil
	}

	gz, err := gzip.NewReader(br)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("bootstrap: 解压失败 %s: %w", path, err)
	}
	return &fileReader{Reader: gz, closers: []io.Closer{gz, f}}, nil
}

// fileReader 把任意读取器与一组待关闭的资源绑在一起，
// 让调用方不必知道这一层套了几层（文件？文件+gzip？）。
type fileReader struct {
	io.Reader
	closers []io.Closer
}

// Close 按打开顺序的逆序关闭。逐个关而不是遇错即返回：
// 关掉 gzip 失败不该让底层文件句柄泄漏。
func (fr *fileReader) Close() error {
	errs := make([]error, 0, len(fr.closers))
	for i := len(fr.closers) - 1; i >= 0; i-- {
		if err := fr.closers[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// readCatalog 逐行解析引导文件，每成功映射一件商品就调用一次 yield。
// 返回成功映射数与跳过数。
//
// 解析与落库分开，是为了让「这份文件到底能不能映射」可以脱离数据库被验证：
// 之前那个导入器的失败方式恰恰是「没有报错、也没有数据」——文件读到了、每行
// 都映射不出来、导入 0 件，一路安静。把这段单独拎出来，测试里直接喂真实
// 文件就能把这种失败钉死。
//
// 单行坏掉（JSON 残缺、没有规格）只跳过该行并计数，不中断整体导入——3,700 行
// 里有一行脏数据不该让整份目录导不进去。
func readCatalog(
	ctx context.Context,
	r io.Reader,
	logger *slog.Logger,
	yield func(productRow) error,
) (imported, skipped int, err error) {
	scanner := bufio.NewScanner(r)
	// 默认 bufio.Scanner 的 token 上限是 64 KiB，catalog-v3.jsonl 单行可能更大
	const maxTokenSize = 2 * 1024 * 1024 // 2 MiB
	scanner.Buffer(make([]byte, maxTokenSize), maxTokenSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// UseNumber 是 price_major 走 json.Number 的前提：不带它，
		// 金额会被先解成 float64，再转字符串就已经带上浮点误差了。
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()

		var p jsonProduct
		if err := dec.Decode(&p); err != nil {
			skipped++
			logger.WarnContext(ctx, "跳过无效 JSON 行",
				slog.String("error", err.Error()))
			continue
		}

		row, err := p.toRow()
		if err != nil {
			skipped++
			logger.WarnContext(ctx, "跳过无法映射的商品行",
				slog.String("product_id", p.ProductID),
				slog.String("error", err.Error()))
			continue
		}

		if err := yield(row); err != nil {
			return imported, skipped, err
		}
		imported++
	}
	if err := scanner.Err(); err != nil {
		return imported, skipped, fmt.Errorf("bootstrap: 读取文件失败: %w", err)
	}
	return imported, skipped, nil
}

// Load 逐行读取引导文件（catalog-v3.jsonl 明文或其 .gz），映射后 upsert 每个
// 商品，返回成功写入的行数。
//
// 「一行都没成功」返回错误而不是安静地报 0：那正是 schema 对不上时的表现
// （文件读到了、每行都映射失败），安静报 0 会让「商品库是空的」变成一个
// 没有报警的已知状态——这个项目已经有过一次。
func (l *BootstrapLoader) Load(ctx context.Context, path string) (int, error) {
	f, err := openCatalogFile(path)
	if err != nil {
		return 0, err
	}
	// 只读文件，关闭失败的后果仅是句柄晚一点归还，不影响已读内容
	defer func() { _ = f.Close() }()

	const batchSize = 500
	var count int
	batch := make([]productRow, 0, batchSize)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := l.upsertBatch(ctx, batch)
		if err != nil {
			return err
		}
		count += n
		batch = batch[:0]
		return nil
	}

	imported, skipped, err := readCatalog(ctx, f, l.logger, func(row productRow) error {
		batch = append(batch, row)
		if len(batch) >= batchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return count, err
	}
	if err := flush(); err != nil {
		return count, err
	}

	l.logger.InfoContext(ctx, "商品引导完成",
		slog.String("path", path),
		slog.Int("imported", imported),
		slog.Int("skipped", skipped))

	if imported == 0 && skipped > 0 {
		return 0, fmt.Errorf(
			"bootstrap: %s 的 %d 行全部映射失败，导入 0 件——大概率是文件结构与 jsonProduct 对不上",
			path, skipped)
	}
	return count, nil
}

// Count 返回 catalog.products 现有行数。
//
// 给「库为空才导入」用：CopyFrom 不做 ON CONFLICT，第二次导入必然整体失败再
// 逐行回退，3,700 行逐条 INSERT 是白白多花的时间。先数一下，非空就跳过。
func (l *BootstrapLoader) Count(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var n int
	if err := l.pool.QueryRow(ctx, `SELECT count(*) FROM catalog.products`).Scan(&n); err != nil {
		return 0, fmt.Errorf("bootstrap: 统计商品数失败: %w", err)
	}
	return n, nil
}

// upsertBatch upserts a batch of products.
func (l *BootstrapLoader) upsertBatch(ctx context.Context, batch []productRow) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 先把 attributes 序列化为 JSON
	type row struct {
		productRow
		AttributesJSON []byte
	}
	rows := make([]row, len(batch))
	for i := range batch {
		attrsJSON, err := json.Marshal(batch[i].Attributes)
		if err != nil {
			return 0, fmt.Errorf("bootstrap: 序列化属性失败: %w", err)
		}
		rows[i] = row{productRow: batch[i], AttributesJSON: attrsJSON}
	}

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
func (l *BootstrapLoader) upsertBatchRowByRow(ctx context.Context, batch []productRow) (int, error) {
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
	// 按下标取址而非值拷贝：productRow 有 23 个字段（约 350 字节），
	// 逐行拷贝整结构体在批量回退路径上是纯浪费，指针只读不改语义。
	for i := range batch {
		p := &batch[i]
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
