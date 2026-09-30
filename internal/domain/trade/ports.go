package trade

import (
	"context"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
)

// InventoryRecord 是一个 SKU 的权威库存行。
//
// 报价（Title / UnitPrice / Currency）与库存同处一行：确认单里展示的金额必须
// 与扣减时校验的金额取自同一条记录，否则「看到的价」与「成交的价」会分叉。
type InventoryRecord struct {
	SKUID          string
	ProductID      string
	Title          string
	Stock          int64
	UnitPriceMinor int64
	Currency       catalog.Currency
}

// SeedSKU 是商品目录里一个待初始化库存的 SKU。
type SeedSKU struct {
	SKUID          string
	ProductID      string
	Title          string
	Stock          int64
	UnitPriceMinor int64
	Currency       catalog.Currency
}

// OrderFilter 是订单列表的过滤与分页条件。
type OrderFilter struct {
	BuyerID string
	// Status 为空表示不过滤。
	Status order.Status
	Offset int
	Limit  int
}

// OrderPage 是订单列表的一页。
//
// Total 是过滤条件下的全量计数，与 Limit 无关：分页控件需要知道总共有多少页，
// 只给出本页条数会让前端无法翻页。
type OrderPage struct {
	Orders []OrderSnapshot
	Total  int
	Offset int
	Limit  int
}

// PrepareRequest 是发起一次交易确认的请求。
//
// 幂等键是 OperationID 而不是「内容」：调用方重试同一个操作时应当传同一个编号，
// 存储层据此判断「这是重试」还是「这是另一笔交易」。
type PrepareRequest struct {
	OperationID string
	BuyerID     string
	SessionID   string
	Action      Action
	// Items 是下单明细，仅 ActionCreate 使用。
	Items []Item
	// ShippingAddress 仅 ActionCreate 使用。
	ShippingAddress order.Address
	// OrderID 与 Reason 仅 ActionCancel 使用。
	OrderID   string
	Reason    string
	ExpiresAt time.Time
}

// Store 是交易账本的持久化端口。
//
// 名字不带包名后缀：调用处写作 trade.Store，而不是 trade.TradeStore。
//
// 实现必须满足下面四条语义，否则账本会出现重复交易或超卖：
//
//  1. 一次 prepare 只在同一个 OperationID 上建立一张确认单；重复调用返回首次
//     建立的那张，而不是新建，且新传入的有效期被忽略。
//  2. prepare 与 resolve 都在单个事务内完成，扣减库存、写订单与写决议要么全成，
//     要么全不成。
//  3. resolve 只有第一次会改变状态；同一决议的重试返回首次结果，
//     相反决议返回 CodeDecisionConflict。
//  4. 库存扣减是带条件的比较并更新：库存不足或报价已变时不得扣减，
//     并分别返回 CodeInsufficientStock 与 CodePriceChanged。
type Store interface {
	// InitializeInventory 用商品目录里的 SKU 初始化或刷新库存。
	//
	// 已存在 SKU 的价格与展示字段会被刷新，但库存数量绝不重置——
	// 重启或目录刷新都不应该把卖掉的货变回来。
	InitializeInventory(ctx context.Context, skus []SeedSKU) error

	// Inventory 返回指定 SKU 的库存；skus 为空表示全部。
	Inventory(ctx context.Context, skus []string) (map[string]int64, error)

	// Prepare 建立或复取一张确认单。
	Prepare(ctx context.Context, req PrepareRequest) (Confirmation, error)

	// Confirmation 按标识取确认单。不存在返回 CodeNotFound。
	Confirmation(ctx context.Context, confirmationID, buyerID, sessionID string) (Confirmation, error)

	// Confirmations 列出当前买家与会话最近的确认单，最多 limit 条、上限 20 条。
	Confirmations(ctx context.Context, buyerID, sessionID string, limit int) ([]Confirmation, error)

	// Resolve 执行用户对确认单的决议。
	Resolve(
		ctx context.Context,
		confirmationID, buyerID, sessionID string,
		snapshotHash string,
		decision Decision,
	) (Confirmation, error)

	// Order 按标识取订单快照。
	Order(ctx context.Context, orderID, buyerID string) (OrderSnapshot, error)

	// Orders 列出当前买家的订单。
	Orders(ctx context.Context, filter OrderFilter) (OrderPage, error)

	// ClaimOrderForCancel 在取消确认生效前把订单标记为已取消并回补库存。
	// 它由 Resolve 在事务内调用，不单独对外。
}

// Clock 提供当前时间，便于测试固定时间推进。
type Clock interface {
	Now() time.Time
}

// OrderSnapshot 是订单的对外快照。
//
// 金额同时给出最小单位与可展示的形式：最小单位是权威值（不丢精度），
// 可展示形式是给买家看的那一份，两者由同一个字段派生，不会各算各的。
type OrderSnapshot struct {
	OrderID          string           `json:"order_id"`
	BuyerID          string           `json:"buyer_id"`
	Status           order.Status     `json:"status"`
	TotalAmountMajor string           `json:"total_amount_major"`
	Currency         catalog.Currency `json:"currency"`
	ShippingAddress  string           `json:"shipping_address"`
	Lines            []LineSnapshot   `json:"lines"`
	CreatedAt        time.Time        `json:"created_at"`
	CancelReason     string           `json:"cancel_reason,omitempty"`
	TotalAmountMinor int64            `json:"total_amount_minor"`
	AmountScope      string           `json:"amount_scope"`
	OrderKind        string           `json:"order_kind"`
}

// LineSnapshot 是订单行的对外快照。
type LineSnapshot struct {
	ProductID      string `json:"product_id"`
	SKUID          string `json:"sku_id"`
	Title          string `json:"title"`
	UnitPriceMajor string `json:"unit_price_major"`
	Quantity       int64  `json:"quantity"`
	// Currency 不在源快照里，但订单行脱离币种没有意义；
	// 保留它不会破坏既有消费方，只是多一个字段。
	Currency catalog.Currency `json:"currency"`
}
