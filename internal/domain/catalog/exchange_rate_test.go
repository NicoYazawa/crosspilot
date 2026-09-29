package catalog

import (
	"errors"
	"testing"

	"github.com/govalues/decimal"
)

func TestNewExchangeRate(t *testing.T) {
	r, err := ParseExchangeRate(CNY, USD, "0.1382")
	if err != nil {
		t.Fatalf("构造汇率失败: %v", err)
	}
	if r.Base != CNY || r.Quote != USD {
		t.Errorf("币种对 = %s/%s，期望 CNY/USD", r.Base, r.Quote)
	}
	if got := r.String(); got != "CNY/USD 0.1382" {
		t.Errorf("String() = %q，期望 %q", got, "CNY/USD 0.1382")
	}
}

func TestNewExchangeRateRejectsBadInput(t *testing.T) {
	t.Run("币种非法", func(t *testing.T) {
		if _, err := NewExchangeRate("cny", USD, decimal.One); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("应返回 ErrInvalidCurrency，得到 %v", err)
		}
		if _, err := NewExchangeRate(CNY, "usd", decimal.One); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("应返回 ErrInvalidCurrency，得到 %v", err)
		}
	})

	t.Run("币种相同", func(t *testing.T) {
		if _, err := NewExchangeRate(USD, USD, decimal.One); !errors.Is(err, ErrSameCurrency) {
			t.Errorf("应返回 ErrSameCurrency，得到 %v", err)
		}
	})

	t.Run("汇率非正", func(t *testing.T) {
		for _, bad := range []decimal.Decimal{decimal.Zero, decimal.NegOne} {
			if _, err := NewExchangeRate(CNY, USD, bad); !errors.Is(err, ErrInvalidRate) {
				t.Errorf("汇率 %s 应返回 ErrInvalidRate，得到 %v", bad, err)
			}
		}
	})

	t.Run("汇率字符串非法", func(t *testing.T) {
		if _, err := ParseExchangeRate(CNY, USD, "加元"); !errors.Is(err, ErrInvalidRate) {
			t.Errorf("应返回 ErrInvalidRate，得到 %v", err)
		}
	})
}

func TestExchangeRateConvert(t *testing.T) {
	r, err := ParseExchangeRate(CNY, USD, "0.1382")
	if err != nil {
		t.Fatal(err)
	}

	got, err := r.Convert(MustMoney("1000.00", CNY))
	if err != nil {
		t.Fatalf("换算失败: %v", err)
	}
	// 1000.00 × 0.1382 = 138.20
	if got.String() != "138.20 USD" {
		t.Errorf("1000 CNY 换算 = %s，期望 138.20 USD", got)
	}

	// 结果收敛到目标币种小数位
	r2, err := ParseExchangeRate(USD, JPY, "150.4567")
	if err != nil {
		t.Fatal(err)
	}
	converted, err := r2.Convert(MustMoney("10.00", USD))
	if err != nil {
		t.Fatalf("换算失败: %v", err)
	}
	// 10.00 × 150.4567 = 1504.567 → 日元零位 → 1505
	if converted.String() != "1505 JPY" {
		t.Errorf("10 USD 换算 = %s，期望 1505 JPY", converted)
	}
}

func TestExchangeRateConvertRejectsWrongCurrency(t *testing.T) {
	r, err := ParseExchangeRate(CNY, USD, "0.1382")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Convert(MustMoney("1.00", EUR)); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("应返回 ErrCurrencyMismatch，得到 %v", err)
	}
}

// TestExchangeRateConvertOverflowPaths 覆盖换算的两条溢出路径：
// 一条由 decimal 相乘的位数上限触发，一条由收敛到目标币种小数位触发。
func TestExchangeRateConvertOverflowPaths(t *testing.T) {
	cases := []struct {
		name   string
		rate   string
		amount string
	}{
		{"相乘即溢出", "1000", "99999999999999999.99"},
		{"收敛到目标小数位时越界", "10000", "99999999999999.99"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := ParseExchangeRate(USD, CNY, tc.rate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Convert(MustMoney(tc.amount, USD)); !errors.Is(err, ErrAmountOverflow) {
				t.Errorf("应返回 ErrAmountOverflow，得到 %v", err)
			}
		})
	}
}

func TestExchangeRateInvertOverflow(t *testing.T) {
	// 极小的汇率取倒数会超出可表示范围
	r, err := ParseExchangeRate(USD, CNY, "0.0000000000000000001")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Invert(); err == nil {
		t.Error("取倒数溢出时应返回错误")
	}
}

func TestExchangeRateInvert(t *testing.T) {
	r, err := ParseExchangeRate(USD, CNY, "7.2450")
	if err != nil {
		t.Fatal(err)
	}

	inv, err := r.Invert()
	if err != nil {
		t.Fatalf("取倒数失败: %v", err)
	}
	if inv.Base != CNY || inv.Quote != USD {
		t.Errorf("反向汇率币种对 = %s/%s，期望 CNY/USD", inv.Base, inv.Quote)
	}

	// 往返一次应回到原值附近：100 USD → CNY → USD
	cny, err := r.Convert(MustMoney("100.00", USD))
	if err != nil {
		t.Fatal(err)
	}
	back, err := inv.Convert(cny)
	if err != nil {
		t.Fatal(err)
	}
	if back.String() != "100.00 USD" {
		t.Errorf("往返换算 = %s，期望 100.00 USD", back)
	}
}
