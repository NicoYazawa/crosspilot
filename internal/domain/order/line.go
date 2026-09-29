package order

import (
	"fmt"

	"github.com/govalues/decimal"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// Line 是订单中的一行：一种商品及其数量。
//
// 小计不存字段而是现算，避免单价、数量与小计三者互相矛盾。
type Line struct {
	ProductID string
	Title     string
	UnitPrice catalog.Money
	Quantity  int
}

// NewLine 构造订单行。数量必须为正，单价币种必须合法。
func NewLine(productID, title string, unitPrice catalog.Money, quantity int) (Line, error) {
	line := Line{
		ProductID: productID,
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
// 相乘结果需要舍入时会按币种小数位收敛并返回成功；溢出则返回错误。
func (l Line) Subtotal() (catalog.Money, error) {
	// 小数位为 0 时任何 int64 都能表示，MustNew 不会 panic；
	// 这一步只是把 int 数量提升为 decimal。
	quantity := decimal.MustNew(int64(l.Quantity), 0)

	subtotal, err := l.UnitPrice.Mul(quantity)
	if err != nil {
		return catalog.Money{}, fmt.Errorf("%w: 商品 %s 小计计算失败: %w",
			ErrInvalidOrder, l.ProductID, err)
	}
	return subtotal, nil
}
