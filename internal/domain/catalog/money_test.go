package catalog

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/govalues/decimal"
)

func TestNewMoneyNormalizesToCurrencyScale(t *testing.T) {
	cases := []struct {
		name   string
		amount string
		cur    Currency
		want   string
	}{
		{"两位币种补零", "1.5", USD, "1.50 USD"},
		{"两位币种舍入", "1.005", USD, "1.00 USD"},
		{"两位币种中点向偶数", "1.125", USD, "1.12 USD"},
		{"两位币种中点向上", "1.135", USD, "1.14 USD"},
		{"零位币种舍入", "1234.6", JPY, "1235 JPY"},
		{"零位币种中点向偶数", "1234.5", JPY, "1234 JPY"},
		{"三位币种", "1.2345", KWD, "1.234 KWD"},
		{"整数金额", "42", USD, "42.00 USD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseMoney(tc.amount, tc.cur)
			if err != nil {
				t.Fatalf("ParseMoney(%q, %s) 报错: %v", tc.amount, tc.cur, err)
			}
			if got := m.String(); got != tc.want {
				t.Errorf("ParseMoney(%q, %s) = %q，期望 %q", tc.amount, tc.cur, got, tc.want)
			}
			if m.Amount.Scale() != tc.cur.Scale() {
				t.Errorf("小数位 = %d，期望 %d", m.Amount.Scale(), tc.cur.Scale())
			}
		})
	}
}

func TestNewMoneyRejectsInvalidCurrency(t *testing.T) {
	for _, bad := range []Currency{"usd", "US", "US1", "", "USDD"} {
		if _, err := NewMoney(decimal.One, bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("币种 %q 应返回 ErrInvalidCurrency，得到 %v", bad, err)
		}
	}
}

func TestParseMoneyRejectsMalformedAmount(t *testing.T) {
	for _, bad := range []string{"", "abc", "1.2.3", "$5", "NaN", "Inf"} {
		if _, err := ParseMoney(bad, USD); !errors.Is(err, ErrInvalidAmount) {
			t.Errorf("金额 %q 应返回 ErrInvalidAmount，得到 %v", bad, err)
		}
	}
}

func TestMustMoneyPanicsOnBadInput(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("非法输入时 MustMoney 应当 panic")
		}
	}()
	MustMoney("not-a-number", USD)
}

func TestMoneyAddAndSub(t *testing.T) {
	a := MustMoney("10.50", USD)
	b := MustMoney("2.25", USD)

	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add 报错: %v", err)
	}
	if sum.String() != "12.75 USD" {
		t.Errorf("10.50 + 2.25 = %s，期望 12.75 USD", sum)
	}

	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("Sub 报错: %v", err)
	}
	if diff.String() != "8.25 USD" {
		t.Errorf("10.50 - 2.25 = %s，期望 8.25 USD", diff)
	}
}

func TestMoneyAddOverflowReturnsError(t *testing.T) {
	largest := MustMoney("99999999999999999.99", USD)
	if _, err := largest.Add(largest); err == nil {
		t.Error("金额溢出时应返回错误，而不是回绕")
	}
}

func TestMoneyCurrencyMismatch(t *testing.T) {
	usd := MustMoney("1.00", USD)
	cny := MustMoney("1.00", CNY)

	if _, err := usd.Add(cny); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("跨币种相加应返回 ErrCurrencyMismatch，得到 %v", err)
	}
	if _, err := usd.Sub(cny); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("跨币种相减应返回 ErrCurrencyMismatch，得到 %v", err)
	}
	if _, err := usd.Cmp(cny); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("跨币种比较应返回 ErrCurrencyMismatch，得到 %v", err)
	}
	if usd.Equal(cny) {
		t.Error("不同币种的等值比较应为 false")
	}
}

func TestMoneyMul(t *testing.T) {
	unit := MustMoney("29.99", USD)

	got, err := unit.Mul(decimal.MustParse("3"))
	if err != nil {
		t.Fatalf("Mul 报错: %v", err)
	}
	if got.String() != "89.97 USD" {
		t.Errorf("29.99 × 3 = %s，期望 89.97 USD", got)
	}

	// 结果收敛到币种小数位：29.99 × 1.005 = 30.13995 → 30.14
	rounded, err := unit.Mul(decimal.MustParse("1.005"))
	if err != nil {
		t.Fatalf("Mul 报错: %v", err)
	}
	if rounded.String() != "30.14 USD" {
		t.Errorf("29.99 × 1.005 = %s，期望 30.14 USD", rounded)
	}
}

func TestMoneyMulStrictRejectsInexactResult(t *testing.T) {
	unit := MustMoney("10.00", USD)

	// 10.00 × 0.3333 = 3.333 在两位小数下必须舍入到 3.33，严格版本应报错
	if _, err := unit.MulStrict(decimal.MustParse("0.3333")); !errors.Is(err, ErrInexactResult) {
		t.Errorf("无法精确表示时应返回 ErrInexactResult，得到 %v", err)
	}

	// 10.00 × 0.333 = 3.33 恰好落在两位小数上，严格版本不应报错
	if _, err := unit.MulStrict(decimal.MustParse("0.333")); err != nil {
		t.Errorf("结果恰好可表示时不应报错，得到 %v", err)
	}

	// 10.00 × 2 可以精确表示
	got, err := unit.MulStrict(decimal.MustParse("2"))
	if err != nil {
		t.Fatalf("MulStrict 报错: %v", err)
	}
	if got.String() != "20.00 USD" {
		t.Errorf("10.00 × 2 = %s，期望 20.00 USD", got)
	}

	// 非严格版本对同一算式应静默收敛到两位小数
	loose, err := unit.Mul(decimal.MustParse("0.3333"))
	if err != nil {
		t.Fatalf("Mul 报错: %v", err)
	}
	if loose.String() != "3.33 USD" {
		t.Errorf("10.00 × 0.3333 = %s，期望 3.33 USD", loose)
	}
}

func TestNewMoneyRejectsOutOfRangeAmount(t *testing.T) {
	// 19 位整数在两位小数下补零会超出可表示范围，必须报错而不是静默截断
	if _, err := NewMoney(decimal.MustParse("9999999999999999999"), USD); !errors.Is(err, ErrAmountOverflow) {
		t.Errorf("超出范围时应返回 ErrAmountOverflow，得到 %v", err)
	}
}

func TestMoneySubOverflowReturnsError(t *testing.T) {
	neg := MustMoney("-99999999999999999.99", USD)
	pos := MustMoney("99999999999999999.99", USD)
	if _, err := neg.Sub(pos); !errors.Is(err, ErrAmountOverflow) {
		t.Errorf("相减溢出时应返回 ErrAmountOverflow，得到 %v", err)
	}
}

// TestMoneyMultiplicationOverflowPaths 覆盖相乘的两条溢出路径：
// 一条由 decimal 自身的位数上限触发，一条由补齐币种小数位触发。
func TestMoneyMultiplicationOverflowPaths(t *testing.T) {
	cases := []struct {
		name    string
		amount  string
		factor  string
		wantErr error
	}{
		{"超出 19 位上限", "99999999999999999.99", "1000", ErrAmountOverflow},
		{"补齐小数位越界", "99999999999999.99", "10000", ErrAmountOverflow},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := MustMoney(tc.amount, USD)
			factor := decimal.MustParse(tc.factor)

			if _, err := base.Mul(factor); !errors.Is(err, tc.wantErr) {
				t.Errorf("Mul 应返回 %v，得到 %v", tc.wantErr, err)
			}
			if _, err := base.MulStrict(factor); !errors.Is(err, tc.wantErr) {
				t.Errorf("MulStrict 应返回 %v，得到 %v", tc.wantErr, err)
			}
		})
	}
}

func TestMoneyNegAndAbs(t *testing.T) {
	m := MustMoney("-12.34", USD)
	if got := m.Neg().String(); got != "12.34 USD" {
		t.Errorf("Neg = %s，期望 12.34 USD", got)
	}
	if got := m.Abs().String(); got != "12.34 USD" {
		t.Errorf("Abs = %s，期望 12.34 USD", got)
	}
	if !m.IsNegative() {
		t.Error("-12.34 应被判为负数")
	}
}

func TestMoneyCmp(t *testing.T) {
	small := MustMoney("1.00", USD)
	large := MustMoney("2.00", USD)

	if got, _ := small.Cmp(large); got != -1 {
		t.Errorf("1.00 vs 2.00 = %d，期望 -1", got)
	}
	if got, _ := large.Cmp(small); got != 1 {
		t.Errorf("2.00 vs 1.00 = %d，期望 1", got)
	}
	same := MustMoney("1.00", USD)
	if got, _ := small.Cmp(same); got != 0 {
		t.Errorf("1.00 vs 1.00 = %d，期望 0", got)
	}
}

func TestMoneyEqualIsScaleSensitive(t *testing.T) {
	a := MustMoney("1.5", USD) // 归一为 1.50
	b := MustMoney("1.50", USD)
	if !a.Equal(b) {
		t.Error("1.5 与 1.50 归一后应相等")
	}
	if a.Amount.String() != b.Amount.String() {
		t.Errorf("归一后表示应一致：%s vs %s", a.Amount, b.Amount)
	}
}

func TestMoneyIsZero(t *testing.T) {
	if !MustMoney("0", USD).IsZero() {
		t.Error("0.00 应判为零")
	}
	if !Zero(USD).IsZero() {
		t.Error("Zero(USD) 应判为零")
	}
	if MustMoney("0.01", USD).IsZero() {
		t.Error("0.01 不应判为零")
	}
}

func TestMoneyJSONRoundTrip(t *testing.T) {
	original := MustMoney("29.99", USD)

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	want := `{"amount":"29.99","currency":"USD"}`
	if string(raw) != want {
		t.Errorf("序列化结果 = %s，期望 %s", raw, want)
	}

	var decoded Money
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if !decoded.Equal(original) {
		t.Errorf("往返后 %s != %s", decoded, original)
	}
}

func TestMoneyUnmarshalRejectsBadInput(t *testing.T) {
	cases := []string{
		`"not-json"`,
		`{"amount":"abc","currency":"USD"}`,
		`{"amount":"1.00","currency":"usd"}`,
		`{"amount":"1.00","currency":""}`,
	}
	for _, bad := range cases {
		var m Money
		if err := json.Unmarshal([]byte(bad), &m); err == nil {
			t.Errorf("%s 应导致反序列化失败", bad)
		}
	}
}

func TestMoneyJSONPadsToCurrencyScale(t *testing.T) {
	raw, err := json.Marshal(MustMoney("5", JPY))
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if want := `{"amount":"5","currency":"JPY"}`; string(raw) != want {
		t.Errorf("序列化结果 = %s，期望 %s", raw, want)
	}
}

func TestToMajorUnits(t *testing.T) {
	m := MustMoney("29.99", USD)
	whole, frac := m.ToMajorUnits()
	if whole != 29 || frac != 99 {
		t.Errorf("ToMajorUnits() = (%d, %d), want (29, 99)", whole, frac)
	}

	// 整元
	m2 := MustMoney("100.00", USD)
	whole2, frac2 := m2.ToMajorUnits()
	if whole2 != 100 || frac2 != 0 {
		t.Errorf("ToMajorUnits() = (%d, %d), want (100, 0)", whole2, frac2)
	}

	// JPY 零位小数
	m3 := MustMoney("1234", JPY)
	whole3, frac3 := m3.ToMajorUnits()
	if whole3 != 1234 || frac3 != 0 {
		t.Errorf("ToMajorUnits() JPY = (%d, %d), want (1234, 0)", whole3, frac3)
	}

	// 负数（frac 部分与 whole 同号）
	m4 := MustMoney("-5.75", USD)
	whole4, frac4 := m4.ToMajorUnits()
	if whole4 != -5 || frac4 != -75 {
		t.Errorf("ToMajorUnits() 负数 = (%d, %d), want (-5, -75)", whole4, frac4)
	}
}
