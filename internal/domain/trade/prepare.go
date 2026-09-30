package trade

import (
	"crypto/hmac"
	"errors"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/order"
)

// 参数长度上限。与线上契约一致：超长的标识不是「长一点的标识」，而是攻击面。
const (
	maxOperationIDLen = 128
	maxBuyerIDLen     = 64
	maxSessionIDLen   = 64
	maxOrderIDLen     = 32
	maxSKUIDLen       = 64
	maxTitleLen       = 255
	maxReasonLen      = 255
	maxConfirmList    = 20
	maxOrderPageSize  = 100
)

// PrepareInput 是发起一次交易确认的输入。
//
// 它把「用户请求」与「本次操作的幂等键」分开：同一个 OperationID 反复出现
// 表示重试，而不是新交易。
type PrepareInput struct {
	OperationID     string
	BuyerID         string
	SessionID       string
	Action          Action
	Items           []Item
	ShippingAddress order.Address
	OrderID         string
	Reason          string
	ExpiresAt       time.Time
}

// PrepareDeps 是 Prepare 需要的外部信息。全部由调用方在事务内一次性取好。
//
// 幂等重放所需的「既有确认单」走 WithExisting 注入，而不是做成一个导出的
// 结构体字段：注入通道只留一条，调用方就不会误以为存在第二种接法。
type PrepareDeps struct {
	Now time.Time
	// Inventory 是按 SKU 索引的权威库存行，必须包含输入里出现的全部 SKU。
	Inventory map[string]InventoryRecord
	// Order 仅在 ActionCancel 时需要：已有的待取消订单。
	Order *OrderView
	// existing 是同一操作编号下已存在的确认单，由存储层查得。
	existing Confirmation
	// existingFound 标记上面那条记录是否真的存在。
	existingFound bool
}

// WithExisting 注入「该操作编号已存在的确认单」，供幂等重放判定使用。
func (d PrepareDeps) WithExisting(confirmation Confirmation) PrepareDeps {
	d.existing = confirmation
	d.existingFound = true
	return d
}

// WithInventory 注入权威库存快照，供报价与库存校验使用。
func (d PrepareDeps) WithInventory(inventory map[string]InventoryRecord) PrepareDeps {
	d.Inventory = inventory
	return d
}

// WithOrder 注入待取消订单的快照。
func (d PrepareDeps) WithOrder(view *OrderView) PrepareDeps {
	d.Order = view
	return d
}

// existingFound 报告是否已经存在同一操作编号的确认单。
func (d PrepareDeps) hasExisting() bool { return d.existingFound }

// OrderView 是取消确认需要比对的历史订单快照。
//
// 它必须携带订单上所有会被取消载荷引用的字段，尤其是收货地址：
// 少带一个字段，那个字段的改动就无法在决议时被发现，
// 「用户批准的订单」与「正在被取消的订单」就不再是同一张。
type OrderView struct {
	OrderID          string
	BuyerID          string
	Status           order.Status
	SKUs             []string
	Items            []Item
	ShippingAddress  order.Address
	Currency         string
	TotalAmountMinor int64
}

// PrepareOutput 是 Prepare 的产物：一张可以直接落库的确认单。
type PrepareOutput struct {
	Confirmation Confirmation
	// Replayed 为真表示这是对既有确认单的重放，存储层不应写入新行。
	Replayed bool
}

// Prepare 执行交易确认的第一阶段：校验、规范化、算摘要，产出一张待决议的确认单。
//
// 纯函数：同样的输入与依赖给同样的输出。数据库只负责把它原子地写下去，
// 因此「校验顺序对不对」这件事可以在单测里穷举，而不需要真库参与。
//
// 顺序是这个函数最重要的部分：
//
//  1. 标识与动作的形状校验；
//  2. 规范化载荷并算请求摘要（只做形状与上限检查，不查库存）；
//  3. 幂等重放判定——同一操作编号已存在时，比较归属与请求摘要后原样返回；
//  4. 有效期检查；
//  5. 报价与库存核对——这一步依赖外部状态，因此必须排在重放判定之后。
//
// 第 3 步先于第 5 步不是风格问题：调用方重试一笔刚刚成功扣减过库存的请求时，
// 价格可能已经变了、库存可能已经不够了，而这些都是这笔交易自己造成的。
// 先做核对就会把「同一次重试」判成「非法请求」，把幂等语义打穿。
func Prepare(input PrepareInput, deps PrepareDeps) (PrepareOutput, error) {
	operationID, err := requiredText(input.OperationID, "operation_id", maxOperationIDLen)
	if err != nil {
		return PrepareOutput{}, err
	}
	buyerID, err := requiredText(input.BuyerID, "buyer_id", maxBuyerIDLen)
	if err != nil {
		return PrepareOutput{}, err
	}
	sessionID, err := requiredText(input.SessionID, "session_id", maxSessionIDLen)
	if err != nil {
		return PrepareOutput{}, err
	}
	if !input.Action.Valid() {
		return PrepareOutput{}, Errorf(CodeInvalidArgument, "不支持的交易操作")
	}
	if input.ExpiresAt.IsZero() {
		return PrepareOutput{}, Errorf(CodeInvalidArgument, "确认有效期必须包含时区")
	}

	payload, err := buildPayload(input, deps)
	if err != nil {
		return PrepareOutput{}, err
	}
	requestHash, err := RequestHash(input.Action, payload)
	if err != nil {
		return PrepareOutput{}, err
	}

	// 幂等重放先于有效期与报价校验：这里比较的是「同一个操作编号下的请求内容」，
	// 归属不符与内容不符分别给出不同错误码，且归属先判——先告诉调用方
	// 「这不是你的确认单」，而不是「你的内容和别人存的不一样」。
	if deps.hasExisting() {
		existing := deps.existing
		if existing.BuyerID != buyerID || existing.SessionID != sessionID {
			return PrepareOutput{}, Errorf(CodeOwnerMismatch, "无权访问此买家或会话的交易确认")
		}
		if existing.RequestHash != requestHash {
			return PrepareOutput{}, Errorf(CodeOperationConflict, "同一操作编号不能更改商品、数量、地址或操作")
		}
		return PrepareOutput{Confirmation: existing, Replayed: true}, nil
	}

	expiry := input.ExpiresAt.UTC()
	if !expiry.After(deps.Now.UTC()) {
		return PrepareOutput{}, Errorf(CodeConfirmationExpired, "确认有效期已过，请重新生成确认")
	}

	// 只有确定要新建确认单时才核对报价与库存
	if err := verifyQuotes(payload, deps.Inventory); err != nil {
		return PrepareOutput{}, err
	}
	applyAuthoritativeTitles(payload, deps.Inventory)

	confirmationID, err := newConfirmationID()
	if err != nil {
		return PrepareOutput{}, err
	}

	confirmation := Confirmation{
		ConfirmationID: confirmationID,
		OperationID:    operationID,
		BuyerID:        buyerID,
		SessionID:      sessionID,
		Action:         input.Action,
		RequestHash:    requestHash,
		Payload:        payload,
		// 有效期归一到 UTC：摘要覆盖它，若保留调用方的时区，
		// 同一时刻在「内存里算一次」与「从数据库读回来再算一次」会得到两个摘要
		// ——数据库读回的时间携带的是数据库会话的时区。
		ExpiresAt: expiry.UTC(),
		Status:    StatusPending,
		CreatedAt: deps.Now.UTC(),
	}
	confirmation.SnapshotHash, err = confirmation.ComputeSnapshotHash()
	if err != nil {
		return PrepareOutput{}, err
	}
	return PrepareOutput{Confirmation: confirmation}, nil
}

// verifyQuotes 逐项核对报价与库存。
//
// 确认单上的金额一旦生成就对买家可见，先展示再校验会让买家看到一个
// 随后被推翻的价格，因此核对必须发生在写库之前。
func verifyQuotes(payload Payload, inventory map[string]InventoryRecord) error {
	if payload.Create == nil {
		return nil
	}
	for _, item := range payload.Create.Items {
		if _, err := quoteOf(inventory, item); err != nil {
			return err
		}
	}
	return nil
}

// applyAuthoritativeTitles 用库存记录里的标题覆盖请求里的标题。
//
// 确认单展示的商品名必须与金额同源：模型给出的标题不可信，
// 而金额来自库存记录，两者不一致会让确认单自相矛盾。
func applyAuthoritativeTitles(payload Payload, inventory map[string]InventoryRecord) {
	if payload.Create == nil {
		return
	}
	for i, item := range payload.Create.Items {
		if record, ok := inventory[item.SKUID]; ok {
			payload.Create.Items[i].Title = TitleOrFallback(record.Title, item.SKUID)
		}
	}
}

// buildPayload 校验请求内容并规范化成载荷。
//
// 它不查库存也不查订单：只回答「这份请求自身是否自洽」。
// 依赖外部状态的部分留给调用方在重放判定之后再做。
func buildPayload(input PrepareInput, deps PrepareDeps) (Payload, error) {
	switch input.Action {
	case ActionCreate:
		return buildCreatePayload(input)
	case ActionCancel:
		return buildCancelPayload(input, deps)
	default:
		return Payload{}, Errorf(CodeInvalidArgument, "不支持的交易操作")
	}
}

func buildCreatePayload(input PrepareInput) (Payload, error) {
	if len(input.Items) == 0 {
		return Payload{}, Errorf(CodeInvalidArgument, "订单至少需要一个商品")
	}
	if err := input.ShippingAddress.Validate(); err != nil {
		return Payload{}, Errorf(CodeInvalidArgument, "缺少完整收货地址")
	}

	items, err := dedupeItems(input.Items)
	if err != nil {
		return Payload{}, err
	}
	ordered, total, currency, err := sortAndTotal(items)
	if err != nil {
		return Payload{}, err
	}

	return Payload{Create: &CreatePayload{
		Items:            ordered,
		ShippingAddress:  trimAddress(input.ShippingAddress),
		Currency:         currency,
		TotalAmountMinor: total,
	}}, nil
}

// buildCancelPayload 由待取消订单重建取消载荷。
//
// 依赖订单快照：取消载荷必须描述「这张订单现在长什么样」，
// 否则决议时无法判断订单在等待期间是否被改动过。
func buildCancelPayload(input PrepareInput, deps PrepareDeps) (Payload, error) {
	orderID, err := requiredText(input.OrderID, "order_id", maxOrderIDLen)
	if err != nil {
		return Payload{}, err
	}
	reason, err := requiredText(input.Reason, "reason", maxReasonLen)
	if err != nil {
		return Payload{}, err
	}
	if deps.Order == nil {
		return Payload{}, Errorf(CodeNotFound, "订单不存在")
	}
	view := *deps.Order
	if view.BuyerID != input.BuyerID {
		return Payload{}, Errorf(CodeOwnerMismatch, "无权访问其他买家的订单")
	}
	if view.OrderID != orderID {
		return Payload{}, Errorf(CodeNotFound, "订单不存在")
	}
	if view.Status != order.StatusConfirmed {
		return Payload{}, Errorf(CodeOrderChanged, "只有已确认且未取消的订单可以发起取消")
	}
	payload, err := CancelPayloadOf(view, reason)
	if err != nil {
		return Payload{}, err
	}
	return Payload{Cancel: &payload}, nil
}

// CancelPayloadOf 由订单快照重建取消载荷。
//
// 取消决议生效前会再用当前订单重建一次并比对：订单在等待决议期间被改动过，
// 就必须拒绝这次取消，而不是按一份过期的快照去补库存。
func CancelPayloadOf(view OrderView, reason string) (CancelPayload, error) {
	if len(view.Items) == 0 {
		return CancelPayload{}, Errorf(CodeInventoryMigrationRequired, "旧订单明细不合法，请核对迁移")
	}
	for _, item := range view.Items {
		if item.Quantity <= 0 || item.SKUID == "" {
			return CancelPayload{}, Errorf(CodeInventoryMigrationRequired, "旧订单明细不合法，请核对迁移")
		}
	}

	ordered, total, currency, err := sortAndTotal(view.Items)
	if err != nil {
		return CancelPayload{}, err
	}
	if view.TotalAmountMinor != 0 && view.TotalAmountMinor != total {
		return CancelPayload{}, Errorf(CodeOrderChanged, "订单内容或状态已变化，请重新生成取消确认")
	}
	if view.Currency != "" && string(currency) != view.Currency {
		return CancelPayload{}, Errorf(CodeOrderChanged, "订单内容或状态已变化，请重新生成取消确认")
	}

	return CancelPayload{
		OrderID:          view.OrderID,
		Reason:           reason,
		OrderStatus:      view.Status,
		Items:            ordered,
		ShippingAddress:  trimAddress(view.ShippingAddress),
		Currency:         currency,
		TotalAmountMinor: total,
	}, nil
}

// CancelPayloadMatches 比对重建出的取消载荷与已存载荷是否一致。
//
// 比的是整个载荷：订单号、原因、状态、明细、地址、金额任意一项变化
// 都意味着「用户批准的那份内容」已不再成立。
func CancelPayloadMatches(stored, rebuilt CancelPayload) bool {
	if stored.OrderID != rebuilt.OrderID ||
		stored.Reason != rebuilt.Reason ||
		stored.OrderStatus != rebuilt.OrderStatus ||
		stored.Currency != rebuilt.Currency ||
		stored.TotalAmountMinor != rebuilt.TotalAmountMinor ||
		stored.ShippingAddress != rebuilt.ShippingAddress ||
		len(stored.Items) != len(rebuilt.Items) {
		return false
	}
	for i := range stored.Items {
		if stored.Items[i] != rebuilt.Items[i] {
			return false
		}
	}
	return true
}

// dedupeItems 校验并合并同一 SKU 的多行。
//
// 同一 SKU 拆成多行是模型的常见输出，按数量相加处理；但价格或商品信息
// 不一致就说明请求自相矛盾，必须拒绝而不是任选一个。
func dedupeItems(items []Item) ([]Item, error) {
	merged := make(map[string]Item, len(items))
	for _, item := range items {
		skuID, err := requiredText(item.SKUID, "sku_id", maxSKUIDLen)
		if err != nil {
			return nil, err
		}
		productID, err := requiredText(item.ProductID, "product_id", maxSKUIDLen)
		if err != nil {
			return nil, err
		}
		if item.Quantity <= 0 {
			return nil, Errorf(CodeInvalidArgument, "quantity 必须为不小于 1 的整数")
		}
		if item.UnitPriceMinor < 0 {
			return nil, Errorf(CodeInvalidArgument, "unit_price_minor 必须为不小于 0 的整数")
		}
		if !item.Currency.Valid() {
			return nil, Errorf(CodeInvalidArgument, "currency 必须为受支持的三位代码，实际 %q", string(item.Currency))
		}
		item.SKUID = skuID
		item.ProductID = productID

		existing, ok := merged[skuID]
		if !ok {
			merged[skuID] = item
			continue
		}
		if !existing.equalTo(item) {
			return nil, Errorf(CodeInvalidArgument, "相同 SKU 的价格或商品信息不一致")
		}
		if err := addQuantity(&existing, item.Quantity); err != nil {
			return nil, err
		}
		merged[skuID] = existing
	}

	out := make([]Item, 0, len(merged))
	for _, item := range merged {
		out = append(out, item)
	}
	return out, nil
}

// quoteOf 在权威库存里核对一行商品的报价与库存。
func quoteOf(inventory map[string]InventoryRecord, item Item) (InventoryRecord, error) {
	record, ok := inventory[item.SKUID]
	if !ok || record.ProductID != item.ProductID {
		return InventoryRecord{}, Errorf(CodeNotFound, "商品 SKU 不存在或尚未初始化持久库存")
	}
	if record.UnitPriceMinor != item.UnitPriceMinor || record.Currency != item.Currency {
		return InventoryRecord{}, Errorf(CodePriceChanged, "商品价格或币种已变化，请重新生成确认")
	}
	if record.Stock < item.Quantity {
		return InventoryRecord{}, Errorf(CodeInsufficientStock, "SKU %s 库存不足", item.SKUID)
	}
	return record, nil
}

// Output 是决议校验的产物。
type Output struct {
	Confirmation Confirmation
	// Decided 为真表示这次决议只是重放已生效的结果，不产生新交易。
	Decided bool
	// Create 与 Cancel 至多有一个非空，表示本次要真正执行的动作。
	Create *CreateExecution
	Cancel *CancelExecution
}

// CreateExecution 描述一次要执行的下单。
type CreateExecution struct {
	BuyerID    string
	Lines      []ExecutionLine
	Address    order.Address
	Currency   string
	TotalMinor int64
}

// ExecutionLine 是执行期的订单行。
type ExecutionLine struct {
	ProductID      string
	SKUID          string
	Title          string
	UnitPriceMinor int64
	Quantity       int64
}

// CancelExecution 描述一次要执行的取消。
type CancelExecution struct {
	BuyerID    string
	OrderID    string
	Reason     string
	Lines      []ExecutionLine
	TotalMinor int64
}

// ResolveInput 是用户对确认单的决议。
type ResolveInput struct {
	ConfirmationID string
	BuyerID        string
	SessionID      string
	SnapshotHash   string
	Decision       Decision
}

// Resolve 执行交易确认的第二阶段：校验决议并产出要执行的动作。
//
// 顺序是这个函数最重要的部分，且每一步都有理由：
//
//  1. 身份与参数校验——不让无权者走到任何分支；
//  2. 快照摘要恒时比对——确认单内容与用户看到的一致；
//  3. 已决议则幂等返回——先于过期检查，否则网络重试会得到「已过期」，
//     而交易其实早已完成；
//  4. 过期检查；
//  5. 执行动作。
//
// 把 3 放到 4 之后就会产生「重试变成第二笔交易」或「重试被误报失败」，
// 这正是交易账本最贵的一类缺陷。
func Resolve(stored Confirmation, input ResolveInput, now time.Time) (Output, error) {
	if _, err := requiredText(input.BuyerID, "buyer_id", maxBuyerIDLen); err != nil {
		return Output{}, err
	}
	if _, err := requiredText(input.SessionID, "session_id", maxSessionIDLen); err != nil {
		return Output{}, err
	}
	if !input.Decision.valid() {
		return Output{}, Errorf(CodeInvalidArgument, "确认决议必须为布尔值")
	}
	if err := validateSnapshotShape(input.SnapshotHash); err != nil {
		return Output{}, err
	}
	if stored.BuyerID != input.BuyerID || stored.SessionID != input.SessionID {
		return Output{}, Errorf(CodeOwnerMismatch, "无权访问此买家或会话的交易确认")
	}

	expected, err := stored.ComputeSnapshotHash()
	if err != nil {
		return Output{}, err
	}
	if !SnapshotMatches(stored.SnapshotHash, expected) || !SnapshotMatches(stored.SnapshotHash, input.SnapshotHash) {
		return Output{}, Errorf(CodeSnapshotMismatch, "确认内容已经变化，请重新查看完整确认")
	}

	target := input.Decision.Status()
	if stored.Status != StatusPending {
		if stored.Status != target {
			return Output{}, Errorf(CodeDecisionConflict, "此确认已经作出另一项决议，不能修改")
		}
		return Output{Confirmation: stored, Decided: true}, nil
	}

	if !stored.ExpiresAt.After(now.UTC()) {
		return Output{}, Errorf(CodeConfirmationExpired, "确认已过期，请重新生成确认")
	}

	if input.Decision == DecisionReject {
		resolved := stored
		resolved.Status = StatusRejected
		at := now.UTC()
		resolved.ResolvedAt = &at
		return Output{Confirmation: resolved}, nil
	}

	switch {
	case stored.Payload.Create != nil:
		execution, err := createExecutionOf(stored)
		if err != nil {
			return Output{}, err
		}
		return Output{Confirmation: stored, Create: execution}, nil
	case stored.Payload.Cancel != nil:
		execution, err := cancelExecutionOf(stored)
		if err != nil {
			return Output{}, err
		}
		return Output{Confirmation: stored, Cancel: execution}, nil
	default:
		return Output{}, Errorf(CodeInvalidArgument, "确认单载荷为空")
	}
}

func createExecutionOf(stored Confirmation) (*CreateExecution, error) {
	payload := stored.Payload.Create
	if err := payload.validate(); err != nil {
		return nil, err
	}
	return &CreateExecution{
		BuyerID:    stored.BuyerID,
		Lines:      executionLines(payload.Items),
		Address:    payload.ShippingAddress,
		Currency:   string(payload.Currency),
		TotalMinor: payload.TotalAmountMinor,
	}, nil
}

func cancelExecutionOf(stored Confirmation) (*CancelExecution, error) {
	payload := stored.Payload.Cancel
	if err := payload.validate(); err != nil {
		return nil, err
	}
	return &CancelExecution{
		BuyerID:    stored.BuyerID,
		OrderID:    payload.OrderID,
		Reason:     payload.Reason,
		Lines:      executionLines(payload.Items),
		TotalMinor: payload.TotalAmountMinor,
	}, nil
}

func executionLines(items []Item) []ExecutionLine {
	out := make([]ExecutionLine, 0, len(items))
	for _, item := range items {
		out = append(out, ExecutionLine{
			ProductID:      item.ProductID,
			SKUID:          item.SKUID,
			Title:          item.Title,
			UnitPriceMinor: item.UnitPriceMinor,
			Quantity:       item.Quantity,
		})
	}
	return out
}

// valid 报告决议是否为已知取值。
func (d Decision) valid() bool {
	return d == DecisionApprove || d == DecisionReject
}

// validateSnapshotShape 检查快照摘要的形状：64 位小写十六进制。
//
// 形状不对与内容不对返回同一个错误码：调用方不需要区分「你编的」与「你改的」。
func validateSnapshotShape(hash string) error {
	const mismatch = "确认内容已经变化，请重新查看完整确认"
	if len(hash) != 64 {
		return Errorf(CodeSnapshotMismatch, "%s", mismatch)
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		isDigit := c >= '0' && c <= '9'
		isLowerHex := c >= 'a' && c <= 'f'
		if !isDigit && !isLowerHex {
			return Errorf(CodeSnapshotMismatch, "%s", mismatch)
		}
	}
	return nil
}

// requiredText 校验必填文本：非空、去空白后非空、不超长。
func requiredText(value, name string, maximum int) (string, error) {
	trimmed := trimSpace(value)
	if trimmed == "" {
		return "", Errorf(CodeInvalidArgument, "%s 必须为非空字符串，最长 %d 字符", name, maximum)
	}
	if len(trimmed) > maximum {
		return "", Errorf(CodeInvalidArgument, "%s 必须为非空字符串，最长 %d 字符", name, maximum)
	}
	return trimmed, nil
}

// SnapshotEqual 是恒时比较的对外别名，供存储层比对重建出的载荷摘要。
func SnapshotEqual(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// ErrNotPending 供调用方判断「确认单已决议」这一情形。
var ErrNotPending = errors.New("trade: 确认单已决议")
