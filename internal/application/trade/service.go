// Package trade 是交易账本的用例服务。
//
// 服务层只承担领域层看不见的三件事：把调用方的「买什么」意图经商品目录解析成
// 权威报价明细；把外部时钟与有效期翻译成确认单的过期时刻；把空值与接线缺失挡在
// 领域层之前。校验、规范化、排序、合并、摘要与幂等语义一律留在 internal/domain/trade
// ——在应用层复制一份，两边只会在某次改动后分叉，而分叉的那一天就是账本出错的那一天。
//
// 本文件里的 trade.X 指领域包 internal/domain/trade：服务自己的类型在本包内不加限定。
//
// 服务不持有全局状态，全部依赖经 New 注入，因此每条分支都能在没有数据库、
// 没有真实时钟的单测里被穷举。
package trade

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	trade "github.com/NicoYazawa/crosspilot/internal/domain/trade"
)

// defaultConfirmationTTL 是未显式配置时的确认有效期。
//
// 太短会让买家还没看清确认单就先过期，太长则会让已经变化的价格与库存在确认单里
// 停留过久。五分钟是「够看完一屏明细」与「不够长到让报价失效」之间的折中。
const defaultConfirmationTTL = 5 * time.Minute

// 确认单列表的条数边界。上限与领域端口的契约一致；下界是因为「最多 0 条」不是一次
// 有意义的查询，调用方给 0 时按「最少一条」处理。
const (
	minConfirmationLimit = 1
	maxConfirmationLimit = 20
)

// Config 是构造用例服务所需的依赖。
//
// 依赖全部由外部注入：服务不自行取系统时间、不自行连数据库，也不自行读配置，
// 因此它可以在毫秒级单测里被穷举，也不会把某一种基础设施固化进业务逻辑。
type Config struct {
	// Store 是交易账本端口，必填。
	Store Store
	// Catalog 是下单所需的商品目录，只有 PlaceOrder 会用到，可以为空。
	Catalog ProductCatalog
	// Clock 提供当前时间，必填。
	Clock Clock
	// ConfirmationTTL 是确认单的有效期；零值或负值取 defaultConfirmationTTL。
	ConfirmationTTL time.Duration
}

// Service 是交易账本的用例服务。
type Service struct {
	store   Store
	catalog ProductCatalog
	clock   Clock
	ttl     time.Duration
}

// New 构造用例服务。
//
// Store 与 Clock 是每一条路径都要用的依赖，缺失时直接拒绝构造：接线错误应当在
// 启动时暴露，而不是在第一次查单时变成一个空接口上的方法调用。
//
// Catalog 不同：只有下单需要商品目录，查单、决议、取消与库存初始化都不需要它。
// 因此没有目录时仍然允许构造服务，代价是 PlaceOrder 返回一条明确的错误。
// 这比让整个服务起不来更可取——只读的账本路径与决议路径不该被一个还没接好的
// 商品目录拖住；反过来，在 New 里拒绝空目录也会迫使调用方为了「暂时不接目录」
// 去造一个永远返回「查无此物」的假目录，那是把接线问题伪装成业务问题。
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil {
		return nil, errors.New("trade: 缺少交易账本端口 Store")
	}
	if cfg.Clock == nil {
		return nil, errors.New("trade: 缺少时钟 Clock")
	}

	ttl := cfg.ConfirmationTTL
	if ttl <= 0 {
		ttl = defaultConfirmationTTL
	}

	return &Service{
		store:   cfg.Store,
		catalog: cfg.Catalog,
		clock:   cfg.Clock,
		ttl:     ttl,
	}, nil
}

// OrderIntent 是一次下单意图：买哪件商品的哪个规格、买几件。
//
// 这里刻意只有标识与数量，没有价格与标题：价格是商品目录的权威事实，
// 让调用方传价格就等于允许调用方定价，确认单上的金额也就失去了意义。
type OrderIntent struct {
	ProductID string
	SKUID     string
	Quantity  int64
}

// PlaceOrderInput 是发起下单确认的输入。
//
// OperationID 是幂等键：调用方重试同一笔请求时必须带同一个编号，账本据此判断
// 「这是重试」而不是「这是另一笔交易」。留空时由服务生成，调用方需要在下一次
// 重试时把它带回来。
type PlaceOrderInput struct {
	OperationID     string
	BuyerID         string
	SessionID       string
	Items           []OrderIntent
	ShippingAddress order.Address
}

// PlaceOrder 把下单意图解析成权威明细，并委托账本建立一张待决议的确认单。
//
// 服务只补齐领域层看不见的部分：目录解析、报价换算、有效期与幂等键。
// 地址合法性、金额自洽、库存与报价是否仍然一致等判断全部由领域层与账本负责，
// 这里重复一遍只会产生第二个真相。
func (s *Service) PlaceOrder(ctx context.Context, input PlaceOrderInput) (trade.Confirmation, error) {
	buyerID, err := required(input.BuyerID, "buyer_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	sessionID, err := required(input.SessionID, "session_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	if len(input.Items) == 0 {
		return trade.Confirmation{}, trade.Errorf(trade.CodeInvalidArgument, "订单至少需要一个商品")
	}
	for i, intent := range input.Items {
		if _, err := required(intent.ProductID, "product_id"); err != nil {
			return trade.Confirmation{}, err
		}
		if _, err := required(intent.SKUID, "sku_id"); err != nil {
			return trade.Confirmation{}, err
		}
		if intent.Quantity <= 0 {
			return trade.Confirmation{}, trade.Errorf(
				trade.CodeInvalidArgument, "items[%d].quantity 必须为不小于 1 的整数", i)
		}
	}

	if s.catalog == nil {
		return trade.Confirmation{}, trade.Errorf(
			trade.CodeInvalidArgument, "商品目录未接入，暂时无法下单")
	}

	intents, err := mergeIntents(input.Items)
	if err != nil {
		return trade.Confirmation{}, err
	}
	items, err := s.resolveItems(ctx, intents)
	if err != nil {
		return trade.Confirmation{}, err
	}
	operationID, err := operationIDOf(input.OperationID)
	if err != nil {
		return trade.Confirmation{}, err
	}

	return s.store.Prepare(ctx, trade.PrepareRequest{
		OperationID:     operationID,
		BuyerID:         buyerID,
		SessionID:       sessionID,
		Action:          trade.ActionCreate,
		Items:           items,
		ShippingAddress: input.ShippingAddress,
		ExpiresAt:       s.clock.Now().Add(s.ttl),
	})
}

// resolveItems 逐条把下单意图换成权威明细。
//
// 价格、标题与币种一律取自目录：调用方说的只是「买什么」，不是「多少钱」。
// 返回的顺序就是 mergeIntents 排好的顺序，账本因此拿到确定的输入。
func (s *Service) resolveItems(ctx context.Context, intents []OrderIntent) ([]trade.Item, error) {
	items := make([]trade.Item, 0, len(intents))
	for _, intent := range intents {
		product, found, err := s.catalog.Find(ctx, intent.ProductID)
		if err != nil {
			// 目录故障原样上抛：服务层无从判断它是「暂时不可用」还是「查无此物」，
			// 换成自己的错误只会把调用方唯一可用的信息抹掉。
			return nil, err
		}
		// 目录返回了另一件商品说明它自己没对上号。此时宁可按「不存在」处理，
		// 也不能照着错误的报价下单。
		if !found || product.ProductID != intent.ProductID {
			return nil, trade.Errorf(trade.CodeNotFound, "商品 %s 不存在", intent.ProductID)
		}

		sku, ok := findSKU(product.SKUs, intent.SKUID)
		if !ok {
			return nil, trade.Errorf(trade.CodeNotFound, "商品 %s 的规格 %s 不存在", intent.ProductID, intent.SKUID)
		}

		unitPriceMinor, err := trade.MinorUnits(sku.Price)
		if err != nil {
			return nil, trade.Wrapf(trade.CodeInvalidArgument, err,
				"商品 %s 的规格 %s 价格无法换算为最小货币单位", intent.ProductID, intent.SKUID)
		}

		items = append(items, trade.Item{
			ProductID: product.ProductID,
			SKUID:     sku.SKUID,
			Title:     fmt.Sprintf("%s（%s）", product.Title, sku.Spec),
			// 单价以最小货币单位交给账本：金额的精度与币种小数位由领域金额类型
			// 决定，账本只认最小单位，换算只在这一处发生。
			UnitPriceMinor: unitPriceMinor,
			Currency:       sku.Price.Currency,
			Quantity:       intent.Quantity,
		})
	}
	return items, nil
}

// findSKU 在商品的规格列表里按标识取一个规格。
func findSKU(skus []CatalogSKU, skuID string) (CatalogSKU, bool) {
	for _, sku := range skus {
		if sku.SKUID == skuID {
			return sku, true
		}
	}
	return CatalogSKU{}, false
}

// intentKey 是合并重复意图所用的键：同一件商品的同一个规格。
type intentKey struct {
	productID string
	skuID     string
}

// mergeIntents 合并重复的下单意图并按 SKU 升序排列。
//
// 合并只做「同一件商品的同一个规格数量相加」这一件事，不碰领域层的明细合并规则：
// 领域层还会核对同 SKU 的价格与标题是否自相矛盾，那是只有拿到权威报价之后才能做的
// 判断，应用层在此刻没有资格替它下结论。
//
// 排序则是为了让交给账本的输入是确定的：模型每次给出的商品顺序都可能不同，
// 顺序不定会让「同一笔操作」在账本里算出两个不同的请求摘要，重试随之变成第二笔交易。
func mergeIntents(intents []OrderIntent) ([]OrderIntent, error) {
	merged := make(map[intentKey]OrderIntent, len(intents))
	for _, intent := range intents {
		key := intentKey{productID: intent.ProductID, skuID: intent.SKUID}
		existing, ok := merged[key]
		if !ok {
			merged[key] = intent
			continue
		}
		if existing.Quantity > math.MaxInt64-intent.Quantity {
			return nil, trade.Errorf(trade.CodeInvalidArgument,
				"商品 %s 的规格 %s 数量超出可表示范围", intent.ProductID, intent.SKUID)
		}
		existing.Quantity += intent.Quantity
		merged[key] = existing
	}

	out := make([]OrderIntent, 0, len(merged))
	for _, intent := range merged {
		out = append(out, intent)
	}
	// 同一 SKU 挂在两件商品下（目录数据可疑）也要有确定的先后，因此再用商品标识兜底。
	sort.Slice(out, func(i, j int) bool {
		if out[i].SKUID != out[j].SKUID {
			return out[i].SKUID < out[j].SKUID
		}
		return out[i].ProductID < out[j].ProductID
	})
	return out, nil
}

// ResolveInput 是买家对确认单作出决议的输入。
type ResolveInput struct {
	ConfirmationID string
	BuyerID        string
	SessionID      string
	// SnapshotHash 是买家看到的那份确认内容的摘要，由调用方原样带回。
	SnapshotHash string
	Approved     bool
}

// Resolve 执行买家对确认单的决议。
//
// Approved 在这里被翻成领域枚举：布尔值在调用点上无法自证含义，「拒绝」与
// 「尚未决议」的区分也会随之丢失，因此服务层只做这一次翻译，决议语义由领域层定义。
func (s *Service) Resolve(ctx context.Context, input ResolveInput) (trade.Confirmation, error) {
	confirmationID, err := required(input.ConfirmationID, "confirmation_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	buyerID, err := required(input.BuyerID, "buyer_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	sessionID, err := required(input.SessionID, "session_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	snapshotHash, err := required(input.SnapshotHash, "snapshot_hash")
	if err != nil {
		return trade.Confirmation{}, err
	}

	return s.store.Resolve(ctx, confirmationID, buyerID, sessionID, snapshotHash, trade.DecisionOf(input.Approved))
}

// CancelOrderInput 是发起取消确认的输入。
type CancelOrderInput struct {
	OperationID string
	BuyerID     string
	SessionID   string
	OrderID     string
	Reason      string
}

// CancelOrder 发起一张取消确认单。
//
// 与下单共用账本的 Prepare：取消同样是「先准备、后决议」的两阶段动作，
// 服务层只负责校验形状、生成幂等键与计算有效期，取消载荷的具体内容
// （订单当前状态、明细、地址快照）由账本在事务内读出来重建。
func (s *Service) CancelOrder(ctx context.Context, input CancelOrderInput) (trade.Confirmation, error) {
	buyerID, err := required(input.BuyerID, "buyer_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	sessionID, err := required(input.SessionID, "session_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	orderID, err := required(input.OrderID, "order_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	reason, err := required(input.Reason, "reason")
	if err != nil {
		return trade.Confirmation{}, err
	}
	operationID, err := operationIDOf(input.OperationID)
	if err != nil {
		return trade.Confirmation{}, err
	}

	return s.store.Prepare(ctx, trade.PrepareRequest{
		OperationID: operationID,
		BuyerID:     buyerID,
		SessionID:   sessionID,
		Action:      trade.ActionCancel,
		OrderID:     orderID,
		Reason:      reason,
		ExpiresAt:   s.clock.Now().Add(s.ttl),
	})
}

// Get 取一张确认单。
func (s *Service) Get(
	ctx context.Context,
	confirmationID, buyerID, sessionID string,
) (trade.Confirmation, error) {
	id, err := required(confirmationID, "confirmation_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	buyer, err := required(buyerID, "buyer_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	session, err := required(sessionID, "session_id")
	if err != nil {
		return trade.Confirmation{}, err
	}
	return s.store.Confirmation(ctx, id, buyer, session)
}

// List 列出当前买家与会话最近的确认单。
//
// limit 被收敛到 1..20：上限来自账本端口对外的契约，服务层不能把它交给调用方
// 随手指定的数值；下界是因为「最多 0 条」不是一次有意义的查询。
// 收敛而不是报错，是让调用方少一次无谓的失败往返——空值在这里没有第二种合理解释。
func (s *Service) List(
	ctx context.Context,
	buyerID, sessionID string,
	limit int,
) ([]trade.Confirmation, error) {
	buyer, err := required(buyerID, "buyer_id")
	if err != nil {
		return nil, err
	}
	session, err := required(sessionID, "session_id")
	if err != nil {
		return nil, err
	}
	return s.store.Confirmations(ctx, buyer, session, clampLimit(limit))
}

// GetOrder 取一张订单快照。
func (s *Service) GetOrder(ctx context.Context, orderID, buyerID string) (trade.OrderSnapshot, error) {
	id, err := required(orderID, "order_id")
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	buyer, err := required(buyerID, "buyer_id")
	if err != nil {
		return trade.OrderSnapshot{}, err
	}
	return s.store.Order(ctx, id, buyer)
}

// ListOrders 按过滤条件列出订单。
//
// 过滤与分页的语义完全由领域端口定义，服务层原样转发、不做任何裁剪：在这里再定一套
// 自己的分页规则，会让调用方同时面对两份契约，而其中一份随实现漂移。
func (s *Service) ListOrders(ctx context.Context, filter trade.OrderFilter) (trade.OrderPage, error) {
	return s.store.Orders(ctx, filter)
}

// InitializeInventory 用商品目录的 SKU 初始化或刷新账本库存。
//
// 原样转发，包括空列表：该动作的幂等与「绝不重置已售库存」语义由账本负责，
// 服务层在此没有可以补充的判断。
func (s *Service) InitializeInventory(ctx context.Context, skus []trade.SeedSKU) error {
	return s.store.InitializeInventory(ctx, skus)
}

// clampLimit 把列表条数收敛到端口契约允许的区间。
func clampLimit(limit int) int {
	if limit < minConfirmationLimit {
		return minConfirmationLimit
	}
	if limit > maxConfirmationLimit {
		return maxConfirmationLimit
	}
	return limit
}

// required 校验必填标识并返回去掉两端空白后的值。
//
// 只做「非空」这一层：长度上限、字符集与规范化属于领域层的职责，
// 在这里重复一遍，只会在两边规则不一致时变成一条谁也解释不清的拒绝。
func required(value, name string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", trade.Errorf(trade.CodeInvalidArgument, "%s 不能为空", name)
	}
	return trimmed, nil
}

// operationIDOf 返回本次操作的幂等键：调用方没给就生成一个。
//
// 生成而不是拒绝，是因为「忘了带幂等键」不该直接变成一次失败的下单；生成的编号
// 同样落在账本的幂等语义上，只是调用方必须把它带回来重试，否则重试会变成新交易。
func operationIDOf(provided string) (string, error) {
	trimmed := strings.TrimSpace(provided)
	if trimmed != "" {
		return trimmed, nil
	}
	id, err := trade.NewOrderID()
	if err != nil {
		return "", fmt.Errorf("trade: 生成操作编号失败: %w", err)
	}
	return id, nil
}
