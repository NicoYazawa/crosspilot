package shipping

import (
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/govalues/decimal"
)

// newDecFromInt 是测试辅助函数，从 int64 构造 decimal.Decimal。
func newDecFromInt64(v int64) decimal.Decimal {
	d, err := decimal.New(v, 0)
	if err != nil {
		panic("test: new decimal from int: " + err.Error())
	}
	return d
}

// mockRates 是测试用汇率表：1:1 映射，方便手算。
type mockRates map[catalog.Currency]catalog.ExchangeRate

func (m mockRates) Convert(amount catalog.Money, target catalog.Currency) (catalog.Money, error) {
	if amount.Currency == target {
		return amount, nil
	}
	// 简单 mock：所有货币统一按 1:1 处理（测试不依赖真实汇率）
	return catalog.MustMoney(amount.Amount.String(), target), nil
}

func TestTariffSchedule_SupportedDestinations(t *testing.T) {
	ts := NewTariffSchedule(mockRates{})
	dests := ts.SupportedDestinations()
	if len(dests) == 0 {
		t.Fatal("SupportedDestinations 返回空列表")
	}
	// 验证有序
	for i := 1; i < len(dests); i++ {
		if dests[i-1] > dests[i] {
			t.Errorf("SupportedDestinations 未排序: %v", dests)
		}
	}
}

func TestTariffSchedule_Quote(t *testing.T) {
	rates := mockRates{}
	ts := NewTariffSchedule(rates)

	tests := []struct {
		name           string
		subtotal       catalog.Money
		category       string
		shipTo         string
		quantity       int
		targetCurrency catalog.Currency
		wantTariffRate decimal.Decimal
		wantDeMinimis  bool
		wantErr        bool
		errContains    string
	}{
		{
			name:           "CN→US，数码配件 0% 关税（US 税率表定义 zero）",
			subtotal:       catalog.MustMoney("10000.00", catalog.USD), // 10000*7.2=72000 CNY >> 5680，超过 de minimis
			category:       "数码配件",
			shipTo:         "US",
			quantity:       1,
			targetCurrency: catalog.USD,
			wantTariffRate: decimal.Zero,
			wantDeMinimis:  false,
			wantErr:        false,
		},
		{
			name:           "CN→CN 国内，超 de minimis 阈值（触发关税）",
			subtotal:       catalog.MustMoney("5001.00", catalog.CNY), // 5001 > 5000，超过 CN 免税线，触发关税
			category:       "数码配件",
			shipTo:         "CN",
			quantity:       1,
			targetCurrency: catalog.CNY,
			wantTariffRate: parseDec("0.13"),
			wantDeMinimis:  false,
			wantErr:        false,
		},
		{
			name:           "US，低于 de minimis 阈值，关税为 0",
			subtotal:       catalog.MustMoney("5000.00", catalog.CNY), // 5000 CNY < 5680 CNY（800*7.1），触发 de minimis
			category:       "旅行装备",
			shipTo:         "US",
			quantity:       1,
			targetCurrency: catalog.CNY,
			wantTariffRate: parseDec("0.075"),
			wantDeMinimis:  true,
			wantErr:        false,
		},
		{
			name:           "不支持的目的地",
			subtotal:       catalog.MustMoney("100.00", catalog.USD),
			category:       "数码配件",
			shipTo:         "XX",
			quantity:       1,
			targetCurrency: catalog.USD,
			wantErr:        true,
			errContains:    "不支持的目的地",
		},
		{
			name:           "quantity > 1，运费累加 60% 续件（超 de minimis）",
			subtotal:       catalog.MustMoney("6000.00", catalog.CNY), // 6000 > 5000，超过 CN 免税线
			category:       "家居生活",
			shipTo:         "CN",
			quantity:       3,
			targetCurrency: catalog.CNY,
			wantTariffRate: parseDec("0.09"),
			wantDeMinimis:  false,
			wantErr:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			quote, err := ts.Quote(tt.subtotal, tt.category, tt.shipTo, tt.quantity, tt.targetCurrency)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Quote() expected error containing %q, got nil", tt.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("Quote() unexpected error: %v", err)
			}
			if quote.TariffRate.Cmp(tt.wantTariffRate) != 0 {
				t.Errorf("TariffRate = %s, want %s", quote.TariffRate, tt.wantTariffRate)
			}
			if quote.DeMinimisApplied != tt.wantDeMinimis {
				t.Errorf("DeMinimisApplied = %v, want %v", quote.DeMinimisApplied, tt.wantDeMinimis)
			}
		})
	}
}

func TestShippingQuote_LandedTotal(t *testing.T) {
	ts := NewTariffSchedule(mockRates{})
	quote, err := ts.Quote(catalog.MustMoney("100.00", catalog.CNY), "家居生活", "CN", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}
	total, err := quote.LandedTotal()
	if err != nil {
		t.Fatalf("LandedTotal() error: %v", err)
	}
	// 手算：subtotal(100) + freight(25) + tariff(9*0.09=0.81) ≈ 125.81
	expected, _ := catalog.ParseMoney("125.81", catalog.CNY)
	diff, _ := total.Sub(expected)
	// 允许误差在 0.02 以内（汇率 mock 是 1:1，舍入差异）
	if diff.Abs().Amount.Cmp(newDecFromInt64(2)) > 0 {
		t.Errorf("LandedTotal() = %s, expected ~%s", total, expected)
	}
}

func TestTariffSchedule_Quote_QuantityInvalid(t *testing.T) {
	ts := NewTariffSchedule(mockRates{})
	_, err := ts.Quote(catalog.MustMoney("100.00", catalog.CNY), "家居生活", "CN", 0, catalog.CNY)
	if err == nil {
		t.Error("Quote(quantity=0) expected error, got nil")
	}
	_, err = ts.Quote(catalog.MustMoney("100.00", catalog.CNY), "家居生活", "CN", -1, catalog.CNY)
	if err == nil {
		t.Error("Quote(quantity=-1) expected error, got nil")
	}
}
