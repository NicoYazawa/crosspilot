package catalog

import (
	"errors"
	"testing"
)

func TestParseCurrency(t *testing.T) {
	cases := []struct {
		in   string
		want Currency
	}{
		{"USD", USD},
		{"usd", USD},
		{"  Eur  ", EUR},
		{"cny", CNY},
	}
	for _, tc := range cases {
		got, err := ParseCurrency(tc.in)
		if err != nil {
			t.Errorf("ParseCurrency(%q) 报错: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseCurrency(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestParseCurrencyRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "US", "USDD", "US1", "U S", "$US", "美金币"} {
		if _, err := ParseCurrency(bad); !errors.Is(err, ErrInvalidCurrency) {
			t.Errorf("ParseCurrency(%q) 应返回 ErrInvalidCurrency，得到 %v", bad, err)
		}
	}
}

func TestCurrencyScale(t *testing.T) {
	cases := map[Currency]int{
		USD: 2, EUR: 2, CNY: 2, GBP: 2,
		JPY: 0, KRW: 0, VND: 0,
		KWD: 3, BHD: 3, OMR: 3, TND: 3,
		"AUD": 2, // 未特殊列出的币种按两位处理
	}
	for cur, want := range cases {
		if got := cur.Scale(); got != want {
			t.Errorf("%s.Scale() = %d，期望 %d", cur, got, want)
		}
	}
}

func TestCurrencyString(t *testing.T) {
	if got := USD.String(); got != "USD" {
		t.Errorf("USD.String() = %q，期望 \"USD\"", got)
	}
}
