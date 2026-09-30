package trade

import (
	"math"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// TestMoneyAndMinorUnitsRoundTrip 覆盖「最小单位 ↔ 领域金额」的精确往返。
//
// 这条链路是确认单金额与实际扣款之间的唯一换算点：任何一点舍入或符号错误，
// 都会让买家看到的价与成交的价分叉，而两者都是整数，错了也看不出来。
func TestMoneyAndMinorUnitsRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		minor int64
		cur   catalog.Currency
		major string
	}{
		{"两位小数币种", 2999, catalog.USD, "29.99 USD"},
		{"零位小数币种", 1200, catalog.JPY, "1200 JPY"},
		{"三位小数币种", 1234, catalog.BHD, "1.234 BHD"},
		{"最小单位的零位币种", 1, catalog.JPY, "1 JPY"},
		{"不足一个主单位", 5, catalog.USD, "0.05 USD"},
		{"不足一个主单位的三位币种", 7, catalog.BHD, "0.007 BHD"},
		{"零金额", 0, catalog.USD, "0.00 USD"},
		{"零金额的零位币种", 0, catalog.JPY, "0 JPY"},
		{"负数金额", -2999, catalog.USD, "-29.99 USD"},
		{"负数金额的三位币种", -1234, catalog.BHD, "-1.234 BHD"},
		{"零位币种的最大可表示金额", math.MaxInt64, catalog.JPY, "9223372036854775807 JPY"},
		{"两位币种的最大可表示金额", math.MaxInt64, catalog.USD, "92233720368547758.07 USD"},
		{"三位币种的最大可表示金额", math.MaxInt64, catalog.BHD, "9223372036854775.807 BHD"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			money, err := Money(tc.minor, tc.cur)
			if err != nil {
				t.Fatalf("Money(%d, %s) 报错: %v", tc.minor, tc.cur, err)
			}
			if got := money.String(); got != tc.major {
				t.Errorf("Money(%d, %s) = %s，期望 %s", tc.minor, tc.cur, got, tc.major)
			}
			if got := money.Amount.Scale(); got != tc.cur.Scale() {
				t.Errorf("小数位 = %d，期望 %d", got, tc.cur.Scale())
			}

			back, err := MinorUnits(money)
			if err != nil {
				t.Fatalf("MinorUnits(%s) 报错: %v", money, err)
			}
			if back != tc.minor {
				t.Errorf("往返后 = %d，期望 %d", back, tc.minor)
			}
		})
	}
}

func TestMoneyRejectsInvalidCurrency(t *testing.T) {
	cases := []catalog.Currency{"usd", "US", "", "USDD", "US1", "美元"}

	for _, bad := range cases {
		t.Run(string(bad), func(t *testing.T) {
			money, err := Money(100, bad)
			wantCode(t, err, CodeInvalidArgument)
			// 失败时必须是零值金额：带币种但不带金额的返回值会让调用方误以为可用。
			if money.Currency != "" {
				t.Errorf("失败时币种 = %q，期望空值", money.Currency)
			}
		})
	}
}

// TestMinorUnitsRejectsOutOfRangeAmounts 覆盖三种越界。
//
// 越界必须报错而不是回绕：回绕会把一笔巨额订单变成零元或负价订单。
func TestMinorUnitsRejectsOutOfRangeAmounts(t *testing.T) {
	cases := []struct {
		name   string
		amount string
		cur    catalog.Currency
	}{
		// 整数部分本身就超过 int64 能表示的范围。
		{"整数部分越界（两位币种）", "99999999999999999.99", catalog.USD},
		{"整数部分越界（负值）", "-99999999999999999.99", catalog.USD},
		{"整数部分越界（零位币种）", "9999999999999999999", catalog.JPY},
		// 整数部分勉强放得下，但补上小数位之后越界。
		{"最小单位补足后越界", "92233720368547758.08", catalog.USD},
		{"负数最小单位补足后越界", "-92233720368547758.09", catalog.USD},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			money := catalog.MustMoney(tc.amount, tc.cur)
			got, err := MinorUnits(money)
			wantCode(t, err, CodeInvalidArgument)
			if got != 0 {
				t.Errorf("失败时应返回 0，得到 %d", got)
			}
		})
	}
}

// TestMoneyIsExactForAllMinorUnitsAroundBoundaries 逐个断言换算没有舍入误差。
//
// 金额用定点构造而不是除法：1/100 这类值在二进制浮点里没有精确表示，
// 反复换算会漂移。这里特意取每一位的进位边界（9/10、99/100、999/1000），
// 因为退位与进位写反时，只有跨位的那几个值会露馅。
func TestMoneyIsExactForAllMinorUnitsAroundBoundaries(t *testing.T) {
	minors := []int64{0, 1, 9, 10, 99, 100, 101, 999, 1000, 1001, 9999, 1 << 40, math.MaxInt64 - 1}

	for _, currency := range []catalog.Currency{catalog.USD, catalog.JPY, catalog.BHD} {
		for _, minor := range minors {
			money, err := Money(minor, currency)
			if err != nil {
				t.Fatalf("Money(%d, %s) 报错: %v", minor, currency, err)
			}
			back, err := MinorUnits(money)
			if err != nil {
				t.Fatalf("MinorUnits(%s) 报错: %v", money, err)
			}
			if back != minor {
				t.Errorf("%s 往返：%d → %s → %d", currency, minor, money, back)
			}
		}
	}
}
