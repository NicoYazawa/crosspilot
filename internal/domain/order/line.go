package order

import (
	"fmt"
	"strings"

	"github.com/govalues/decimal"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// Line 是订单中的一行：一个 SKU 及其数量。
//
// 小计不存字段而是现算，避免单价、数量与小计三者互相矛盾。
// 单价是下单时刻的价格快照，商品后续调价不影响已落库的订单。
type Line struct {
	ProductID string
	SKUID     string
	Title     string
	UnitPrice catalog.Money
	Quantity  int
}

// NewLine 构造订单行。数量必须为正，单价币种必须合法。
func NewLine(productID, skuID, title string, unitPrice catalog.Money, quantity int) (Line, error) {
	line := Line{
		ProductID: productID,
		SKUID:     skuID,
		Title:     title,
		UnitPrice: unitPrice,
		Quantity:  quantity,
	}
	if err := line.Validate(); err != nil {
		return Line{}, err
	}
	return line, nil
}

// Validate 报告订单行是否满足约束。
func (l Line) Validate() error {
	if l.ProductID == "" {
		return fmt.Errorf("%w: 订单行商品标识为空", ErrInvalidOrder)
	}
	if strings.TrimSpace(l.SKUID) == "" {
		return fmt.Errorf("%w: 商品 %s 缺少规格标识", ErrInvalidOrder, l.ProductID)
	}
	if l.Quantity <= 0 {
		return fmt.Errorf("%w: 商品 %s 数量必须为正，实际 %d",
			ErrInvalidOrder, l.ProductID, l.Quantity)
	}
	if !l.UnitPrice.Currency.Valid() {
		return fmt.Errorf("%w: 商品 %s 单价币种非法", ErrInvalidOrder, l.ProductID)
	}
	if l.UnitPrice.IsNegative() {
		return fmt.Errorf("%w: 商品 %s 单价为负", ErrInvalidOrder, l.ProductID)
	}
	return nil
}

// Subtotal 返回该行小计，即单价乘以数量。
//
// 用 MulStrict 而不是 Mul：订单总额必须与单价、数量分毫不差地对得上，
// 静默舍入会让订单总额与逐行相加的结果不一致。数量是整数，凡需要舍入
// 都说明单价精度与币种小数位不匹配，属于数据问题而不是可接受的近似。
func (l Line) Subtotal() (catalog.Money, error) {
	quantity := decimal.MustNew(int64(l.Quantity), 0)

	subtotal, err := l.UnitPrice.MulStrict(quantity)
	if err != nil {
		return catalog.Money{}, fmt.Errorf("%w: 商品 %s 小计计算失败: %w",
			ErrInvalidOrder, l.ProductID, err)
	}
	return subtotal, nil
}
