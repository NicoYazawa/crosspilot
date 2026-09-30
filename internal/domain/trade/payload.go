package trade

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/govalues/decimal"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
)

// Payload 是确认单要执行的规范化内容。
//
// 决议时重新读回并据此执行，而不是在决议时重算：用户批准的是「他看到的这份内容」，
// 重算等于允许内容在批准与执行之间变化。
type Payload struct {
	// Create 非空表示这是下单确认。
	Create *CreatePayload
	// Cancel 非空表示这是取消确认。
	Cancel *CancelPayload
}

// CreatePayload 是下单确认的载荷。
type CreatePayload struct {
	Items            []Item
	ShippingAddress  order.Address
	Currency         catalog.Currency
	TotalAmountMinor int64
}

// CancelPayload 是取消确认的载荷。
//
// 它保留了下单时的订单快照：决议执行时会用它与数据库里的当前订单逐项比对，
// 订单在等待决议期间发生变化（例如已被另一次取消处理）就拒绝执行。
type CancelPayload struct {
	OrderID          string
	Reason           string
	OrderStatus      order.Status
	Items            []Item
	ShippingAddress  order.Address
	Currency         catalog.Currency
	TotalAmountMinor int64
}

// Item 是确认单里的一行商品。金额以最小货币单位（分）表示。
type Item struct {
	ProductID      string           `json:"product_id"`
	SKUID          string           `json:"sku_id"`
	Title          string           `json:"title"`
	UnitPriceMinor int64            `json:"unit_price_minor"`
	Currency       catalog.Currency `json:"currency"`
	Quantity       int64            `json:"quantity"`
}

// Key 返回该行的唯一标识，用于合并与排序。
func (i Item) Key() string { return i.SKUID }

// equalTo 报告两行除数量外是否一致。同一 SKU 被拆成多行时，
// 只要价格或商品信息不一致就说明请求本身自相矛盾。
func (i Item) equalTo(o Item) bool {
	return i.ProductID == o.ProductID &&
		i.SKUID == o.SKUID &&
		i.Title == o.Title &&
		i.UnitPriceMinor == o.UnitPriceMinor &&
		i.Currency == o.Currency
}

// Action 返回载荷对应的动作。
func (p Payload) Action() Action {
	if p.Cancel != nil {
		return ActionCancel
	}
	return ActionCreate
}

// Validate 报告载荷自身是否自洽。它不查库存，也不查价格是否仍有效。
func (p Payload) Validate() error {
	switch {
	case p.Create != nil && p.Cancel != nil:
		return Errorf(CodeInvalidArgument, "确认单载荷不能同时是下单与取消")
	case p.Create != nil:
		return p.Create.validate()
	case p.Cancel != nil:
		return p.Cancel.validate()
	default:
		return Errorf(CodeInvalidArgument, "确认单载荷为空")
	}
}

func (c *CreatePayload) validate() error {
	if len(c.Items) == 0 {
		return Errorf(CodeInvalidArgument, "订单至少需要一个商品")
	}
	if err := c.ShippingAddress.Validate(); err != nil {
		return Errorf(CodeInvalidArgument, "缺少完整收货地址")
	}
	if err := validateItems(c.Items, c.Currency, c.TotalAmountMinor); err != nil {
		return err
	}
	return nil
}

func (c *CancelPayload) validate() error {
	if c.OrderID == "" {
		return Errorf(CodeInvalidArgument, "取消确认缺少订单号")
	}
	if strings.TrimSpace(c.Reason) == "" {
		return Errorf(CodeInvalidArgument, "取消确认缺少原因")
	}
	if !c.OrderStatus.Valid() {
		return Errorf(CodeInvalidArgument, "订单状态 %q 非法", string(c.OrderStatus))
	}
	if len(c.Items) == 0 {
		return Errorf(CodeInvalidArgument, "取消确认缺少订单明细")
	}
	if err := validateItems(c.Items, c.Currency, c.TotalAmountMinor); err != nil {
		return err
	}
	return nil
}

// validateItems 校验明细与总额自洽。总额必须是逐行金额之和，
// 否则确认单上展示的金额与实际执行的不是同一回事。
func validateItems(items []Item, currency catalog.Currency, totalMinor int64) error {
	if !currency.Valid() {
		return Errorf(CodeInvalidArgument, "币种 %q 非法", string(currency))
	}

	var sum int64
	for _, item := range items {
		if strings.TrimSpace(item.ProductID) == "" {
			return Errorf(CodeInvalidArgument, "商品标识为空")
		}
		if strings.TrimSpace(item.SKUID) == "" {
			return Errorf(CodeInvalidArgument, "规格标识为空")
		}
		if item.Quantity <= 0 {
			return Errorf(CodeInvalidArgument, "商品 %s 数量必须为正整数", item.SKUID)
		}
		if item.UnitPriceMinor < 0 {
			return Errorf(CodeInvalidArgument, "商品 %s 单价不能为负", item.SKUID)
		}
		if item.Currency != currency {
			return Errorf(CodeInvalidArgument, "一张订单的商品币种必须相同")
		}
		if item.UnitPriceMinor > 0 && item.Quantity > math.MaxInt64/item.UnitPriceMinor {
			return Errorf(CodeInvalidArgument, "商品 %s 金额超出可表示范围", item.SKUID)
		}
		lineTotal := item.UnitPriceMinor * item.Quantity
		if sum > math.MaxInt64-lineTotal {
			return Errorf(CodeInvalidArgument, "订单总额超出可表示范围")
		}
		sum += lineTotal
	}

	if sum != totalMinor {
		return Errorf(CodeInvalidArgument, "订单总额与逐行金额之和不一致：%d 与 %d", totalMinor, sum)
	}
	return nil
}

// canonical 把载荷还原成参与哈希的通用映射。
//
// 走一次 JSON 往返而不是手写映射：手写映射一旦漏字段，摘要就会与内容脱钩，
// 而漏字段不会让任何测试失败——它只会让哈希悄悄失去意义。
// 成员标签就是线上字段名，因此往返后的键名与对外契约一致。
func (p Payload) canonical() map[string]any {
	encoded, err := json.Marshal(p)
	if err != nil {
		// 载荷全部由可序列化的基本类型组成，失败说明代码有 bug，
		// 但不能 panic：交易路径上的 panic 会连带回滚整个事务。
		return map[string]any{}
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	// 数字保持为 json.Number：退化成 float64 会让大额金额在往返中改变取值，
	// 而摘要必须是精确的。
	decoder.UseNumber()

	var out map[string]any
	if err := decoder.Decode(&out); err != nil {
		return map[string]any{}
	}
	return out
}

// MarshalJSON 实现 json.Marshaler，只为载荷之一输出字段，
// 避免另一个分支的 null 字段混入线上内容与摘要。
func (p Payload) MarshalJSON() ([]byte, error) {
	switch {
	case p.Create != nil && p.Cancel != nil:
		return nil, Errorf(CodeInvalidArgument, "确认单载荷不能同时是下单与取消")
	case p.Create != nil:
		return json.Marshal(p.Create)
	case p.Cancel != nil:
		return json.Marshal(p.Cancel)
	default:
		return nil, Errorf(CodeInvalidArgument, "确认单载荷为空")
	}
}

// UnmarshalJSON 实现 json.Unmarshaler。
//
// 用 order_id 是否存在来区分两种载荷：取消确认必须带订单号，下单确认不带。
// 解析失败一律返回带码的错误，不静默产出空载荷——空载荷一旦被执行，
// 就是一张没有内容的订单。
func (p *Payload) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("null")) {
		return Errorf(CodeInvalidArgument, "确认单载荷为空")
	}

	var probe struct {
		OrderID string `json:"order_id"`
		Items   []Item `json:"items"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return Errorf(CodeInvalidArgument, "确认单载荷不是合法对象")
	}

	if probe.OrderID != "" {
		var cancel CancelPayload
		if err := json.Unmarshal(trimmed, &cancel); err != nil {
			return Errorf(CodeInvalidArgument, "取消确认载荷解析失败")
		}
		p.Create, p.Cancel = nil, &cancel
		return nil
	}

	// 既没有订单号又没有明细，说明这段 JSON 不是一张确认单载荷。
	// 在这里就拒绝，而不是留到执行阶段：解析出来的空载荷一旦被当作
	// 「下单、零个商品」执行，写下的就是一张没有内容的订单。
	if len(probe.Items) == 0 {
		return Errorf(CodeInvalidArgument, "确认单载荷为空")
	}

	var create CreatePayload
	if err := json.Unmarshal(trimmed, &create); err != nil {
		return Errorf(CodeInvalidArgument, "下单确认载荷解析失败")
	}
	p.Create, p.Cancel = &create, nil
	return nil
}

// createWire 是下单确认的线上表示。成员顺序无关紧要，
// 摘要按成员名排序，不按声明顺序。
type createWire struct {
	Items            []Item           `json:"items"`
	ShippingAddress  addressWire      `json:"shipping_address"`
	Currency         catalog.Currency `json:"currency"`
	TotalAmountMinor int64            `json:"total_amount_minor"`
	AmountScope      string           `json:"amount_scope"`
	OrderKind        string           `json:"order_kind"`
}

// cancelWire 是取消确认的线上表示。
type cancelWire struct {
	OrderID          string           `json:"order_id"`
	Reason           string           `json:"reason"`
	OrderStatus      order.Status     `json:"order_status"`
	Items            []Item           `json:"items"`
	ShippingAddress  addressWire      `json:"shipping_address"`
	Currency         catalog.Currency `json:"currency"`
	TotalAmountMinor int64            `json:"total_amount_minor"`
	AmountScope      string           `json:"amount_scope"`
	OrderKind        string           `json:"order_kind"`
}

// addressWire 是收货地址的线上表示。
type addressWire struct {
	Recipient  string `json:"recipient"`
	Phone      string `json:"phone"`
	Country    string `json:"country"`
	Province   string `json:"province"`
	City       string `json:"city"`
	Line1      string `json:"line1"`
	Line2      string `json:"line2"`
	PostalCode string `json:"postal_code"`
}

// MarshalJSON 让载荷按线上表示序列化，摘要与存储因此共用同一套字段名。
func (c CreatePayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(createWire{
		Items:            c.Items,
		ShippingAddress:  wireOfAddress(c.ShippingAddress),
		Currency:         c.Currency,
		TotalAmountMinor: c.TotalAmountMinor,
		AmountScope:      AmountScope,
		OrderKind:        OrderKind,
	})
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (c *CreatePayload) UnmarshalJSON(data []byte) error {
	var wire createWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	c.Items = wire.Items
	c.ShippingAddress = wire.ShippingAddress.address()
	c.Currency = wire.Currency
	c.TotalAmountMinor = wire.TotalAmountMinor
	return nil
}

// MarshalJSON 实现 json.Marshaler。
func (c CancelPayload) MarshalJSON() ([]byte, error) {
	return json.Marshal(cancelWire{
		OrderID:          c.OrderID,
		Reason:           c.Reason,
		OrderStatus:      c.OrderStatus,
		Items:            c.Items,
		ShippingAddress:  wireOfAddress(c.ShippingAddress),
		Currency:         c.Currency,
		TotalAmountMinor: c.TotalAmountMinor,
		AmountScope:      AmountScope,
		OrderKind:        OrderKind,
	})
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (c *CancelPayload) UnmarshalJSON(data []byte) error {
	var wire cancelWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	c.OrderID = wire.OrderID
	c.Reason = wire.Reason
	c.OrderStatus = wire.OrderStatus
	c.Items = wire.Items
	c.ShippingAddress = wire.ShippingAddress.address()
	c.Currency = wire.Currency
	c.TotalAmountMinor = wire.TotalAmountMinor
	return nil
}

func wireOfAddress(a order.Address) addressWire {
	return addressWire{
		Recipient:  a.Recipient,
		Phone:      a.Phone,
		Country:    a.Country,
		Province:   a.Province,
		City:       a.City,
		Line1:      a.Line1,
		Line2:      a.Line2,
		PostalCode: a.PostalCode,
	}
}

func (w addressWire) address() order.Address {
	return order.Address{
		Recipient:  w.Recipient,
		Phone:      w.Phone,
		Country:    w.Country,
		Province:   w.Province,
		City:       w.City,
		Line1:      w.Line1,
		Line2:      w.Line2,
		PostalCode: w.PostalCode,
	}
}

// Money 把一笔最小单位金额转成领域金额，供订单构造使用。
//
// 走 decimal 的定点构造而不是除法：小数位由币种决定，中间不出现浮点，
// 也不产生需要舍入的中间结果。
func Money(minor int64, currency catalog.Currency) (catalog.Money, error) {
	if !currency.Valid() {
		return catalog.Money{}, Errorf(CodeInvalidArgument, "币种 %q 非法", string(currency))
	}

	amount, err := decimal.New(minor, currency.Scale())
	if err != nil {
		return catalog.Money{}, Errorf(CodeInvalidArgument, "金额 %d 超出可表示范围", minor)
	}
	return catalog.NewMoney(amount, currency)
}

// MinorUnits 把领域金额转回最小货币单位（分）。金额越界返回错误。
//
// 用定点库的整数分解而不是乘法或除法：按币种小数位取出整数部分与小数部分，
// 不引入任何舍入。小数位为 0 的币种（JPY 等）整数部分本身就是最小单位。
func MinorUnits(m catalog.Money) (int64, error) {
	scale := m.Currency.Scale()
	whole, frac, ok := m.Amount.Int64(scale)
	if !ok {
		return 0, Errorf(CodeInvalidArgument, "金额 %s 超出可表示范围", m.String())
	}

	multiplier := int64(1)
	for i := 0; i < scale; i++ {
		multiplier *= 10
	}
	if whole > math.MaxInt64/multiplier || whole < math.MinInt64/multiplier {
		return 0, Errorf(CodeInvalidArgument, "金额 %s 超出可表示范围", m.String())
	}
	minor := whole * multiplier
	if frac > 0 && minor > math.MaxInt64-frac {
		return 0, Errorf(CodeInvalidArgument, "金额 %s 超出可表示范围", m.String())
	}
	if frac < 0 && minor < math.MinInt64-frac {
		return 0, Errorf(CodeInvalidArgument, "金额 %s 超出可表示范围", m.String())
	}
	return minor + frac, nil
}

// TitleOrFallback 返回标题，缺省时用 SKU 标识兜底，避免展示空白。
func TitleOrFallback(title, skuID string) string {
	if strings.TrimSpace(title) == "" {
		return skuID
	}
	return title
}

// String 便于日志与错误信息展示。
func (p Payload) String() string {
	switch {
	case p.Create != nil:
		return fmt.Sprintf("下单确认：%d 行，%d %s",
			len(p.Create.Items), p.Create.TotalAmountMinor, p.Create.Currency)
	case p.Cancel != nil:
		return fmt.Sprintf("取消确认：订单 %s", p.Cancel.OrderID)
	default:
		return "确认单载荷为空"
	}
}
