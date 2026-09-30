package trade

import (
	"context"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	trade "github.com/NicoYazawa/crosspilot/internal/domain/trade"
)

// ProductCatalog 是下单所需的商品读模型端口。
//
// 只声明服务真正会调用的一个方法：接口窄一分，替换实现与写测试替身的代价就少一分。
// 目录的检索、推荐等能力下单并不需要，因此不在这里出现——一个「顺手加上」的方法
// 会让所有实现与替身都被迫跟进。
type ProductCatalog interface {
	// Find 返回商品的权威信息。商品不存在时返回 found 为 false 且 err 为 nil；
	// 只有查询本身失败（后端不可用等）才返回错误。
	Find(ctx context.Context, productID string) (CatalogProduct, bool, error)
}

// CatalogProduct 是下单所需的商品读模型：一件商品及其全部规格报价。
//
// 为什么不用 catalog.Product：领域里的 Product 是面向检索与展示的模型，只有单一
// Price，既没有规格也没有库存。而确认单的每一行必须钉在「某个规格的价格」上——
// 同一件商品的不同规格价格不同，用商品级价格下单就是按错误的价格成交。
//
// 与其给领域模型塞进一个它此刻并不需要的规格结构（那会牵动所有既有的目录消费方），
// 不如在应用层声明服务需要的最小读模型：目录实现负责把自己的数据映射过来，
// 服务层因此不依赖任何具体的目录形状，换成关系库、向量库或远程接口都不影响这里。
type CatalogProduct struct {
	ProductID string
	Title     string
	SKUs      []CatalogSKU
}

// CatalogSKU 是商品下一个规格的权威报价与库存。
//
// Price 用领域金额类型而不是最小单位整数：币种小数位、精度与溢出规则由 catalog.Money
// 统一承担，服务层只负责在交给账本前换成最小单位一次。
type CatalogSKU struct {
	SKUID string
	Spec  string
	Price catalog.Money
	Stock int64
}

// Clock 提供当前时间。
//
// 时间必须可注入：确认单的有效期会进入快照摘要，只有把「现在」变成输入，
// 测试才能断言「恰好到期」这类边界，而这类边界正是账本最贵缺陷的藏身处。
type Clock interface {
	Now() time.Time
}

// Store 是服务用到的交易账本端口。
//
// 它是 trade.TradeStore 的子集，只含服务真正调用的方法。理由有两条：
//
//   - 服务应当依赖「我要用的能力」，而不是「账本恰好有的全部能力」。
//     账本新增一个方法时，服务与它的测试替身不该被迫跟着改。
//   - 端口越窄，替身能实现错的地方就越少；替身越小，测试里能藏的错误越少。
//
// 因此服务只声明这七个方法，不 import 任何持久化实现。
type Store interface {
	// InitializeInventory 用商品目录的 SKU 初始化或刷新库存。
	InitializeInventory(ctx context.Context, skus []trade.SeedSKU) error

	// Prepare 建立或复取一张确认单。
	Prepare(ctx context.Context, req trade.PrepareRequest) (trade.Confirmation, error)

	// Confirmation 按标识取确认单。
	Confirmation(ctx context.Context, confirmationID, buyerID, sessionID string) (trade.Confirmation, error)

	// Confirmations 列出当前买家与会话最近的确认单，最多 limit 条。
	Confirmations(ctx context.Context, buyerID, sessionID string, limit int) ([]trade.Confirmation, error)

	// Resolve 执行用户对确认单的决议。
	Resolve(
		ctx context.Context,
		confirmationID, buyerID, sessionID string,
		snapshotHash string,
		decision trade.Decision,
	) (trade.Confirmation, error)

	// Order 按标识取订单快照。
	Order(ctx context.Context, orderID, buyerID string) (trade.OrderSnapshot, error)

	// Orders 列出当前买家的订单。
	Orders(ctx context.Context, filter trade.OrderFilter) (trade.OrderPage, error)
}

// 编译期确认领域端口至少提供上面这些能力。
//
// 断言写成「账本端口满足 Store」而不是断言 *pg.Store：应用层因此不需要 import
// 任何基础设施包（架构规则也只允许它依赖领域层），而 *pg.Store 既然已经实现了
// trade.Store（见 internal/infra/persistence/pg），就自动满足 Store。
//
// 放在这里而不是装配根：领域端口一旦把某个方法改名或删掉，应该在编译本包时就失败，
// 而不是等到装配根里出现一个 nil 接口、在第一次下单时变成运行期崩溃。
var _ Store = (trade.Store)(nil)
