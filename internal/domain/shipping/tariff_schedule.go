package shipping

import (
	"fmt"
	"math"
	"sort"

	"github.com/govalues/decimal"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// parseDec is a helper that wraps decimal.Parse and panics on error (for constants).
func parseDec(s string) decimal.Decimal {
	d, err := decimal.Parse(s)
	if err != nil {
		panic("shipping: parse decimal constant: " + err.Error())
	}
	return d
}

// 目的国 → 品类 → 关税费率（未列品类走 "*" 兜底）
var _tariffRates = map[string]map[string]decimal.Decimal{
	"CN": {"数码配件": parseDec("0.13"), "旅行装备": parseDec("0.09"), "户外运动": parseDec("0.09"), "家居生活": parseDec("0.09"), "*": parseDec("0.09")},
	"US": {"数码配件": decimal.Zero, "旅行装备": parseDec("0.075"), "户外运动": parseDec("0.075"), "家居生活": parseDec("0.05"), "*": parseDec("0.06")},
	"EU": {"*": parseDec("0.12")},
	"JP": {"*": parseDec("0.08")},
	"SG": {"*": parseDec("0.07")},
}

// 目的国免税额度（CNY 分）
var _deMinimisCNYMinor = map[string]int64{
	"CN": 5_000_00,  // 5000 元
	"US": 800 * 710, // 800 USD * 7.10
	"EU": 150 * 780,
	"JP": 10_000 * 5, // 简化口径
	"SG": 400 * 530,
}

// 目的国基础运费（CNY 分，单件）
var _baseFreightCNYMinor = map[string]int64{
	"CN": 25_00,
	"US": 65_00,
	"EU": 75_00,
	"JP": 45_00,
	"SG": 40_00,
}

// ExchangeRateTable 汇率表接口。
type ExchangeRateTable interface {
	Convert(m catalog.Money, targetCurrency catalog.Currency) (catalog.Money, error)
}

// TariffSchedule 持有汇率表并提供关税报价能力。
type TariffSchedule struct {
	rates ExchangeRateTable
}

// NewTariffSchedule 构造 TariffSchedule。
func NewTariffSchedule(rates ExchangeRateTable) *TariffSchedule {
	return &TariffSchedule{rates: rates}
}

// ShippingQuote 跨境到手价三要素：商品小计、运费、关税。
type ShippingQuote struct {
	ShipTo           catalog.Money
	Subtotal         catalog.Money
	Freight          catalog.Money
	Tariff           catalog.Money
	TariffRate       decimal.Decimal
	DeMinimisApplied bool
}

// LandedTotal 返回到手总价：商品小计 + 运费 + 关税。
func (q ShippingQuote) LandedTotal() (catalog.Money, error) {
	total, err := q.Subtotal.Add(q.Freight)
	if err != nil {
		return catalog.Money{}, err
	}
	return total.Add(q.Tariff)
}

// ToDict 返回可序列化的字典表示。
func (q ShippingQuote) ToDict() map[string]any {
	landed, _ := q.LandedTotal()
	return map[string]any{
		"ship_to":            q.ShipTo.String(),
		"subtotal_major":     q.Subtotal.Amount.String(),
		"freight_major":      q.Freight.Amount.String(),
		"tariff_major":       q.Tariff.Amount.String(),
		"tariff_rate":        q.TariffRate.String(),
		"de_minimis_applied": q.DeMinimisApplied,
		"landed_total_major": landed.Amount.String(),
		"currency":           q.Subtotal.Currency.String(),
	}
}

// SupportedDestinations 返回支持的目的地国家/地区代码列表（按字母序）。
func (ts *TariffSchedule) SupportedDestinations() []string {
	dests := make([]string, 0, len(_tariffRates))
	for k := range _tariffRates {
		dests = append(dests, k)
	}
	sort.Strings(dests)
	return dests
}

// Quote 计算跨境到手价。
func (ts *TariffSchedule) Quote(
	subtotal catalog.Money,
	category string,
	shipTo string,
	quantity int,
	targetCurrency catalog.Currency,
) (ShippingQuote, error) {
	if quantity <= 0 {
		return ShippingQuote{}, fmt.Errorf("shipping: quantity 必须为正整数，当前为 %d", quantity)
	}

	rateTable, ok := _tariffRates[shipTo]
	if !ok {
		return ShippingQuote{}, fmt.Errorf("%w: %s（支持 %v）", ErrUnsupportedDestination, shipTo, ts.SupportedDestinations())
	}

	// 1. 商品小计折算为目标币种
	subtotalTarget, err := ts.rates.Convert(subtotal, targetCurrency)
	if err != nil {
		return ShippingQuote{}, fmt.Errorf("shipping: 折算小计时出错: %w", err)
	}

	// 2. 运费计算（首件全价 + 续件 60%）
	freightFactor := 1.0 + 0.6*float64(quantity-1)
	freightFactorDec, err := decimal.Parse(fmt.Sprintf("%g", freightFactor))
	if err != nil {
		return ShippingQuote{}, fmt.Errorf("shipping: 构造运费因子时出错: %w", err)
	}

	baseFreightMinor := _baseFreightCNYMinor[shipTo]
	baseFreightAmount, err := decimal.New(baseFreightMinor, 2) // 分转元，scale=2
	if err != nil {
		return ShippingQuote{}, fmt.Errorf("shipping: 构造基础运费时出错: %w", err)
	}
	freightAmount, err := baseFreightAmount.Mul(freightFactorDec)
	if err != nil {
		return ShippingQuote{}, fmt.Errorf("shipping: 计算运费时出错: %w", err)
	}
	freightCNY, err := catalog.NewMoney(freightAmount, catalog.CNY)
	if err != nil {
		return ShippingQuote{}, fmt.Errorf("shipping: 构造运费金额时出错: %w", err)
	}
	freightTarget, err := ts.rates.Convert(freightCNY, targetCurrency)
	if err != nil {
		return ShippingQuote{}, fmt.Errorf("shipping: 折算运费时出错: %w", err)
	}

	// 3. 关税计算
	tariffRate, hasCat := rateTable[category]
	if !hasCat {
		tariffRate = rateTable["*"]
	}
	if tariffRate.IsZero() {
		// 零税率：商品本身不收关税，不涉及 de minimis 豁免
		return ShippingQuote{
			ShipTo:           freightTarget,
			Subtotal:         subtotalTarget,
			Freight:          freightTarget,
			Tariff:           catalog.Zero(targetCurrency),
			TariffRate:       tariffRate,
			DeMinimisApplied: false,
		}, nil
	}

	subtotalCNY, err := ts.rates.Convert(subtotal, catalog.CNY)
	if err != nil {
		return ShippingQuote{}, fmt.Errorf("shipping: 折算为 CNY 时出错: %w", err)
	}

	// 将 subtotalCNY 转为整数分
	subtotalMinor, err := moneyToMinorInt64(subtotalCNY)
	if err != nil {
		return ShippingQuote{}, fmt.Errorf("shipping: 获取整数分时出错: %w", err)
	}

	deMinimisMinor := _deMinimisCNYMinor[shipTo]
	var tariffCNY catalog.Money
	deMinimisApplied := false
	if subtotalMinor <= deMinimisMinor {
		tariffCNY = catalog.Zero(catalog.CNY)
		deMinimisApplied = true
	} else {
		taxableMinor := subtotalMinor - deMinimisMinor
		taxableDec, err := decimal.New(taxableMinor, 0)
		if err != nil {
			return ShippingQuote{}, fmt.Errorf("shipping: 构造计税金额时出错: %w", err)
		}
		taxableMoney, err := catalog.NewMoney(taxableDec, catalog.CNY)
		if err != nil {
			return ShippingQuote{}, fmt.Errorf("shipping: 构造计税金额时出错: %w", err)
		}
		tariffCNY, err = taxableMoney.Mul(tariffRate)
		if err != nil {
			return ShippingQuote{}, fmt.Errorf("shipping: 计算关税时出错: %w", err)
		}
		_ = math.Float64frombits(0) // silence unused import
	}

	tariffTarget, err := ts.rates.Convert(tariffCNY, targetCurrency)
	if err != nil {
		return ShippingQuote{}, fmt.Errorf("shipping: 折算关税时出错: %w", err)
	}

	return ShippingQuote{
		ShipTo:           freightTarget,
		Subtotal:         subtotalTarget,
		Freight:          freightTarget,
		Tariff:           tariffTarget,
		TariffRate:       tariffRate,
		DeMinimisApplied: deMinimisApplied,
	}, nil
}

// moneyToMinorInt64 将 Money 转为整数分。
func moneyToMinorInt64(m catalog.Money) (int64, error) {
	whole, frac, ok := m.Amount.Int64(m.Currency.Scale())
	if !ok {
		return 0, fmt.Errorf("shipping: 金额 %s 无法转为整数分", m.Amount.String())
	}
	return whole*100 + frac, nil
}
