package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// Catalog 从 catalog.products 读出下单所需的商品读模型。
//
// 为什么这里只有一个规格：0003 迁移只建了 catalog.products 一张表，规格信息
// 并不在库里——`default_sku_id` 指向商品自身的默认规格，价格也只有一个
// `price_amount_major`。与其在适配器里把不存在的多规格「造」出来（那会让
// 确认单上的价格失去事实依据），不如如实返回单规格；等 SKU 表落地后，
// 这里改成 JOIN 即可，调用方（trade.Service）不受影响。
type Catalog struct {
	pool *pgxpool.Pool
}

// NewCatalog 构造目录读适配器。
func NewCatalog(pool *pgxpool.Pool) *Catalog {
	return &Catalog{pool: pool}
}

// Find 实现 application/trade.ProductCatalog。
//
// 方法签名按结构匹配、不 import 那个包：Go 的隐式接口实现让适配器只依赖
// 领域类型就够了，infra 不必认识 application。
//
// 商品不存在时返回 found=false 且 err=nil：目录里没有这件商品是一个正常答案，
// 不是故障。只有查询本身失败（连接断开、SQL 出错）才返回错误——把两者混成
// 一个错误会让上层无法区分「该给用户 404」和「该给自己报警」。
func (c *Catalog) Find(ctx context.Context, productID string) (catalog.OrderableProduct, bool, error) {
	const q = `
		SELECT id, title, price_amount_major, price_currency,
		       COALESCE(default_sku_id, ''), attributes
		FROM catalog.products
		WHERE id = $1`

	var (
		id, title, amount, currency, skuID string
		attrsJSON                          []byte
	)
	err := c.pool.QueryRow(ctx, q, productID).
		Scan(&id, &title, &amount, &currency, &skuID, &attrsJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return catalog.OrderableProduct{}, false, nil
	}
	if err != nil {
		return catalog.OrderableProduct{}, false, fmt.Errorf("pg: 查询商品 %s 失败: %w", productID, err)
	}

	// 库里的价格是十进制字符串，交给 Money 解析——币种小数位、精度与溢出规则
	// 由领域层统一承担，适配器不做任何数值转换。
	cur := catalog.Currency(currency)
	price, err := catalog.ParseMoney(amount, cur)
	if err != nil {
		return catalog.OrderableProduct{}, false, fmt.Errorf("pg: 商品 %s 的价格 %q 无法解析: %w", productID, amount, err)
	}

	// default_sku_id 为空时退回商品 ID：确认单的每一行必须钉在一个规格上，
	// 没有规格标识就没有可以落库存记录的位置。
	if skuID == "" {
		skuID = id
	}

	return catalog.OrderableProduct{
		ProductID: id,
		Title:     title,
		SKUs:      orderableSKUs(attrsJSON, cur, skuID, price),
	}, true, nil
}

// orderableSKUs 从 attributes.skus 还原全部规格，缺失时退回单条默认规格。
//
// 为什么必须返回**全部**规格：账本侧 trade_sku_inventory 是按规格建的行
// （导入时 7,105 条），下单要拿 sku_id 去核对报价与库存（见 domain/trade/
// prepare.go 的 quoteOf）。只返回默认规格时，一件商品的两个规格里只有第一个
// 能下单，第二个会被判成「规格不存在」——目录里明明有，下单却查无此规格。
//
// 兜底那一条不是编造：库里的 price_amount_major 与 default_sku_id 是权威列，
// 老数据没有 attributes.skus 时它们就是这件商品唯一可下单的规格。
func orderableSKUs(
	attrsJSON []byte,
	cur catalog.Currency,
	defaultSKUID string,
	defaultPrice catalog.Money,
) []catalog.OrderableSKU {
	fallback := []catalog.OrderableSKU{{
		SKUID: defaultSKUID,
		Spec:  "默认规格",
		Price: defaultPrice,
	}}
	if len(attrsJSON) == 0 {
		return fallback
	}

	var attrs map[string]any
	if err := json.Unmarshal(attrsJSON, &attrs); err != nil {
		// 属性列解不出来不该让下单整体失败：权威价格与默认规格都还在，
		// 少掉的是「还有别的规格可选」，退回单规格是保守且正确的降级。
		return fallback
	}

	skus, err := catalog.SKUsFromAttributes(attrs, cur)
	if err != nil || len(skus) == 0 {
		// 同上：规格明细坏掉或缺席时退回单规格，不阻断这件商品的下单。
		return fallback
	}

	out := make([]catalog.OrderableSKU, 0, len(skus))
	for _, sku := range skus {
		out = append(out, catalog.OrderableSKU{
			SKUID: sku.ID,
			Spec:  sku.Spec,
			Price: sku.Price,
			Stock: int64(sku.Stock),
		})
	}
	return out
}
