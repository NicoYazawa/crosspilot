package catalog

import (
	"encoding/json"
	"fmt"

	"github.com/govalues/decimal"
)

// Money 是一笔指定币种的金额。
//
// 金额是十进制定点数而非二进制浮点：0.1、29.99 这类十进制小数在二进制浮点里
// 没有精确表示，反复相加会漂移，是财务系统的典型事故来源。
//
// 不变式：Amount 的小数位恒等于该币种的小数位（见 Currency.Scale）。
// 所有构造与运算入口都会强制这一点，因此「数值相同但表示不同」的歧义不存在，
// 相等判断与持久化都不需要额外归一。
//
// 溢出与精度损失都会返回错误，绝不静默降级或回绕。
//
// Money 是值类型，可安全并发读取。不要用 == 比较金额，用 Equal 或 Cmp。
type Money struct {
	Amount   decimal.Decimal
	Currency Currency
}

// normalize 把金额收敛到指定小数位，并确认收敛后确实落在该精度上。
//
// decimal 的 Rescale 在补零会超出可表示范围时会静默返回原值（小数位保持不变），
// 调用方无从察觉。这里把这个沉默的降级转成显式错误。
func normalize(amount decimal.Decimal, scale int) (decimal.Decimal, error) {
	rescaled := amount.Rescale(scale)
	if rescaled.Scale() != scale {
		return decimal.Decimal{}, fmt.Errorf(
			"%w: 金额 %s 超出 %d 位小数可表示的范围", ErrAmountOverflow, amount.String(), scale)
	}
	return rescaled, nil
}

// NewMoney 构造金额，并收敛到该币种的标准小数位。
// 币种非法返回 ErrInvalidCurrency，金额超出该精度可表示范围返回 ErrAmountOverflow。
func NewMoney(amount decimal.Decimal, c Currency) (Money, error) {
	if !c.Valid() {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidCurrency, string(c))
	}
	normalized, err := normalize(amount, c.Scale())
	if err != nil {
		return Money{}, err
	}
	return Money{Amount: normalized, Currency: c}, nil
}

// ParseMoney 从十进制字符串构造金额，例如 ParseMoney("29.99", USD)。
func ParseMoney(s string, c Currency) (Money, error) {
	amount, err := decimal.Parse(s)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	return NewMoney(amount, c)
}

// MustMoney 是 ParseMoney 的 panic 版本，供包级常量与测试使用。
func MustMoney(s string, c Currency) Money {
	m, err := ParseMoney(s, c)
	if err != nil {
		panic(err)
	}
	return m
}

// Zero 返回该币种的零金额。
func Zero(c Currency) Money {
	return Money{Amount: decimal.Zero.Rescale(c.Scale()), Currency: c}
}

// Add 返回两笔同币种金额之和。
// 币种不同返回 ErrCurrencyMismatch，结果超出可表示范围返回 ErrAmountOverflow。
func (m Money) Add(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	sum, err := m.Amount.AddExact(o.Amount, m.Currency.Scale())
	if err != nil {
		return Money{}, fmt.Errorf("%w: %w", ErrAmountOverflow, err)
	}
	return Money{Amount: sum, Currency: m.Currency}, nil
}

// Sub 返回两笔同币种金额之差。
// 币种不同返回 ErrCurrencyMismatch，结果超出可表示范围返回 ErrAmountOverflow。
func (m Money) Sub(o Money) (Money, error) {
	if m.Currency != o.Currency {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	diff, err := m.Amount.SubExact(o.Amount, m.Currency.Scale())
	if err != nil {
		return Money{}, fmt.Errorf("%w: %w", ErrAmountOverflow, err)
	}
	return Money{Amount: diff, Currency: m.Currency}, nil
}

// Mul 返回金额乘以一个数量系数（件数、月数等），币种不变。
// 结果按币种小数位做一次舍入，规则为四舍六入五成双。
func (m Money) Mul(factor decimal.Decimal) (Money, error) {
	product, err := m.Amount.Mul(factor)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %w", ErrAmountOverflow, err)
	}
	normalized, err := normalize(product, m.Currency.Scale())
	if err != nil {
		return Money{}, err
	}
	return Money{Amount: normalized, Currency: m.Currency}, nil
}

// MulStrict 是 Mul 的严格版本：只要结果需要舍入才能存进币种小数位，就返回
// ErrInexactResult 而不是静默收敛。
//
// 适用于「单价 × 数量必须分毫不差」的账务场景，例如对账、开票金额计算。
func (m Money) MulStrict(factor decimal.Decimal) (Money, error) {
	product, err := m.Amount.Mul(factor)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %w", ErrAmountOverflow, err)
	}
	normalized, err := normalize(product, m.Currency.Scale())
	if err != nil {
		return Money{}, err
	}
	if normalized.Cmp(product) != 0 {
		return Money{}, fmt.Errorf("%w: %s × %s 在 %d 位小数下无法精确表示",
			ErrInexactResult, m.Amount.String(), factor.String(), m.Currency.Scale())
	}
	return Money{Amount: normalized, Currency: m.Currency}, nil
}

// Neg 返回金额的相反数。
func (m Money) Neg() Money {
	return Money{Amount: m.Amount.Neg(), Currency: m.Currency}
}

// Abs 返回金额的绝对值。
func (m Money) Abs() Money {
	return Money{Amount: m.Amount.Abs(), Currency: m.Currency}
}

// Cmp 比较两笔同币种金额，返回 -1、0 或 1。币种不同返回 ErrCurrencyMismatch。
func (m Money) Cmp(o Money) (int, error) {
	if m.Currency != o.Currency {
		return 0, fmt.Errorf("%w: %s <> %s", ErrCurrencyMismatch, m.Currency, o.Currency)
	}
	return m.Amount.Cmp(o.Amount), nil
}

// Equal 报告两笔金额的币种与数值是否都相等。不同币种一律不相等。
func (m Money) Equal(o Money) bool {
	return m.Currency == o.Currency && m.Amount.Equal(o.Amount)
}

// IsZero 报告金额是否为零。
func (m Money) IsZero() bool { return m.Amount.IsZero() }

// IsNegative 报告金额是否小于零。
func (m Money) IsNegative() bool { return m.Amount.IsNeg() }

// String 实现 fmt.Stringer，输出如 "29.99 USD"。
func (m Money) String() string {
	return fmt.Sprintf("%s %s", m.Amount.String(), m.Currency)
}

// moneyJSON 是 Money 的线上表示。金额编码为十进制字符串而不是 JSON number，
// 避免消费端用二进制浮点解析时重新引入精度误差。
type moneyJSON struct {
	Amount   string   `json:"amount"`
	Currency Currency `json:"currency"`
}

// MarshalJSON 实现 json.Marshaler。
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(moneyJSON{Amount: m.Amount.String(), Currency: m.Currency})
}

// UnmarshalJSON 实现 json.Unmarshaler。
// 币种或金额非法时返回错误，不静默兜底。
func (m *Money) UnmarshalJSON(data []byte) error {
	var raw moneyJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("catalog: 解析金额失败: %w", err)
	}
	parsed, err := ParseMoney(raw.Amount, raw.Currency)
	if err != nil {
		return err
	}
	*m = parsed
	return nil
}
