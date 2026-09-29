package catalog

import (
	"fmt"

	"github.com/govalues/decimal"
)

// ExchangeRate 表示两个币种之间的兑换关系：1 单位 Base 可兑 Quote 的数额。
//
// Rate 是十进制定点数，与金额同一套精度规则，避免换算过程引入二进制误差。
type ExchangeRate struct {
	Base  Currency
	Quote Currency
	Rate  decimal.Decimal
}

// NewExchangeRate 构造汇率。
// 要求两个币种代码合法且互不相同，汇率必须为正。
func NewExchangeRate(base, quote Currency, rate decimal.Decimal) (ExchangeRate, error) {
	if !base.Valid() {
		return ExchangeRate{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(base))
	}
	if !quote.Valid() {
		return ExchangeRate{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(quote))
	}
	if base == quote {
		return ExchangeRate{}, fmt.Errorf("%w: %s", ErrSameCurrency, base)
	}
	if !rate.IsPos() {
		return ExchangeRate{}, fmt.Errorf("%w: %s", ErrInvalidRate, rate.String())
	}
	return ExchangeRate{Base: base, Quote: quote, Rate: rate}, nil
}

// ParseExchangeRate 从十进制字符串构造汇率，例如 ParseExchangeRate(CNY, USD, "0.1382")。
func ParseExchangeRate(base, quote Currency, rate string) (ExchangeRate, error) {
	parsed, err := decimal.Parse(rate)
	if err != nil {
		return ExchangeRate{}, fmt.Errorf("%w: %q", ErrInvalidRate, rate)
	}
	return NewExchangeRate(base, quote, parsed)
}

// Convert 把一笔 Base 币种金额换算为 Quote 币种，结果收敛到 Quote 的小数位。
// 传入金额的币种与 Base 不一致时返回 ErrCurrencyMismatch。
func (r ExchangeRate) Convert(m Money) (Money, error) {
	if m.Currency != r.Base {
		return Money{}, fmt.Errorf("%w: 汇率为 %s→%s，金额为 %s",
			ErrCurrencyMismatch, r.Base, r.Quote, m.Currency)
	}

	converted, err := m.Amount.Mul(r.Rate)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %w", ErrAmountOverflow, err)
	}
	normalized, err := normalize(converted, r.Quote.Scale())
	if err != nil {
		return Money{}, err
	}
	return Money{Amount: normalized, Currency: r.Quote}, nil
}

// Invert 返回反向汇率，即 1 单位 Quote 兑 Base 的数额。
func (r ExchangeRate) Invert() (ExchangeRate, error) {
	inverted, err := decimal.One.Quo(r.Rate)
	if err != nil {
		return ExchangeRate{}, fmt.Errorf("catalog: 汇率取倒数失败: %w", err)
	}
	return ExchangeRate{Base: r.Quote, Quote: r.Base, Rate: inverted}, nil
}

// String 实现 fmt.Stringer，输出如 "CNY/USD 0.1382"。
func (r ExchangeRate) String() string {
	return fmt.Sprintf("%s/%s %s", r.Base, r.Quote, r.Rate.String())
}
