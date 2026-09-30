package trade

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
)

// newConfirmationID 生成确认单标识：32 位小写十六进制。
func newConfirmationID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("trade: 生成确认单标识失败: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// NewOrderID 生成订单标识：32 位小写十六进制。
func NewOrderID() (string, error) {
	return newConfirmationID()
}

// trimSpace 去掉两端空白。用标准库实现，避免自己写出漏掉全角空白的版本。
func trimSpace(s string) string { return strings.TrimSpace(s) }

// trimAddress 规范化收货地址：所有字段去两端空白。
//
// 校验发生在规范化之后：带空白的「非空」地址会在展示时露馅，
// 也会让两份内容相同的地址算出不同摘要。
func trimAddress(a order.Address) order.Address {
	return order.Address{
		Recipient:  trimSpace(a.Recipient),
		Phone:      trimSpace(a.Phone),
		Country:    trimSpace(a.Country),
		Province:   trimSpace(a.Province),
		City:       trimSpace(a.City),
		Line1:      trimSpace(a.Line1),
		Line2:      trimSpace(a.Line2),
		PostalCode: trimSpace(a.PostalCode),
	}
}

// addQuantity 把两行数量相加，溢出返回错误而不是回绕。
func addQuantity(target *Item, delta int64) error {
	if delta <= 0 {
		return Errorf(CodeInvalidArgument, "quantity 必须为不小于 1 的整数")
	}
	if target.Quantity > math.MaxInt64-delta {
		return Errorf(CodeInvalidArgument, "商品 %s 数量超出可表示范围", target.SKUID)
	}
	target.Quantity += delta
	return nil
}

// sortAndTotal 按 SKU 升序排列明细并计算总额。
//
// 排序是摘要稳定性的前提：模型给出的商品顺序每次都可能不同，
// 若按原顺序计算摘要，同一份内容会被判成两份，「重试」随之变成「另一笔交易」。
func sortAndTotal(items []Item) ([]Item, int64, catalog.Currency, error) {
	if len(items) == 0 {
		return nil, 0, "", Errorf(CodeInvalidArgument, "订单至少需要一个商品")
	}

	ordered := make([]Item, len(items))
	copy(ordered, items)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].SKUID < ordered[j].SKUID })

	currency := ordered[0].Currency
	if !currency.Valid() {
		return nil, 0, "", Errorf(CodeInvalidArgument, "currency 必须为受支持的三位代码，实际 %q", string(currency))
	}

	var total int64
	for _, item := range ordered {
		if item.Currency != currency {
			return nil, 0, "", Errorf(CodeInvalidArgument, "一张订单的商品币种必须相同")
		}
		if item.UnitPriceMinor > 0 && item.Quantity > math.MaxInt64/item.UnitPriceMinor {
			return nil, 0, "", Errorf(CodeInvalidArgument, "商品 %s 金额超出可表示范围", item.SKUID)
		}
		lineTotal := item.UnitPriceMinor * item.Quantity
		if total > math.MaxInt64-lineTotal {
			return nil, 0, "", Errorf(CodeInvalidArgument, "订单总额超出可表示范围")
		}
		total += lineTotal
	}
	return ordered, total, currency, nil
}
