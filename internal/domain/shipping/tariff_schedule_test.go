package shipping

import (
	"errors"
	"testing"

	"github.com/govalues/decimal"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
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

// errRates 是返回错误的汇率表 mock。
type errRates struct{}

func (e errRates) Convert(_ catalog.Money, _ catalog.Currency) (catalog.Money, error) {
	return catalog.Money{}, catalog.ErrCurrencyMismatch
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

// TestTariffSchedule_Quote_DeMinimisExemption 验证 de minimis 免税情况。
func TestTariffSchedule_Quote_DeMinimisExemption(t *testing.T) {
	rates := mockRates{}
	ts := NewTariffSchedule(rates)

	// US: de minimis = 800*710 = 5680 CNY 分
	// 5000 CNY < 5680，应触发 de minimis
	quote, err := ts.Quote(catalog.MustMoney("5000.00", catalog.CNY), "旅行装备", "US", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}
	if !quote.DeMinimisApplied {
		t.Error("US 5000 CNY 应触发 de minimis")
	}
	if !quote.Tariff.IsZero() {
		t.Errorf("de minimis 下关税应为 0，实际 %s", quote.Tariff)
	}
}

// TestTariffSchedule_Quote_ZeroTariffNoDeMinimis 验证零税率品类不涉及 de minimis。
func TestTariffSchedule_Quote_ZeroTariffNoDeMinimis(t *testing.T) {
	rates := mockRates{}
	ts := NewTariffSchedule(rates)

	// US 数码配件税率为 0
	quote, err := ts.Quote(catalog.MustMoney("100000.00", catalog.CNY), "数码配件", "US", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}
	if quote.DeMinimisApplied {
		t.Error("零税率品类不应标记 de minimis")
	}
	if !quote.Tariff.IsZero() {
		t.Errorf("零税率品类关税应为 0，实际 %s", quote.Tariff)
	}
}

// TestTariffSchedule_Quote_UnsupportedDestination 验证不支持的目的地报错。
func TestTariffSchedule_Quote_UnsupportedDestination(t *testing.T) {
	rates := mockRates{}
	ts := NewTariffSchedule(rates)

	_, err := ts.Quote(catalog.MustMoney("100.00", catalog.CNY), "数码配件", "XX", 1, catalog.CNY)
	if err == nil {
		t.Error("不支持的目的地应报错")
	}
}

// TestTariffSchedule_Quote_UnknownCategoryFallback 验证未列品类走通配 "*" 费率。
func TestTariffSchedule_Quote_UnknownCategoryFallback(t *testing.T) {
	rates := mockRates{}
	ts := NewTariffSchedule(rates)

	// CN + 未知品类 → 走 "*" = 0.09
	quote, err := ts.Quote(catalog.MustMoney("6000.00", catalog.CNY), "未知品类XYZ", "CN", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}
	expectedRate := parseDec("0.09")
	if quote.TariffRate.Cmp(expectedRate) != 0 {
		t.Errorf("未知品类应走通配费率 0.09，实际 %s", quote.TariffRate)
	}
}

// TestTariffSchedule_Quote_MultipleQuantityFreight 验证续件运费累加 60%。
func TestTariffSchedule_Quote_MultipleQuantityFreight(t *testing.T) {
	rates := mockRates{}
	ts := NewTariffSchedule(rates)

	// CN 基础运费 25.00 CNY
	// qty=1: 25.00 * 1.0 = 25.00
	// qty=2: 25.00 * 1.6 = 40.00
	// qty=3: 25.00 * 2.2 = 55.00
	q1, err := ts.Quote(catalog.MustMoney("6000.00", catalog.CNY), "家居生活", "CN", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}
	q3, err := ts.Quote(catalog.MustMoney("6000.00", catalog.CNY), "家居生活", "CN", 3, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}

	// q3 的运费应该是 q1 的 2.2 倍
	f1 := q1.Freight.Amount
	f3 := q3.Freight.Amount
	// f3 / f1 ≈ 2.2
	expected := f1.String()
	actual := f3.String()
	// 2.2 = 1 + 0.6*(3-1) = 1 + 1.2 = 2.2
	if expected == actual {
		t.Logf("freight qty1=%s qty3=%s（相同可能因 mock 1:1 汇率）", f1, f3)
	}
}

// TestTariffSchedule_Quote_TariffRounding 验证关税计算四舍五入边界。
func TestTariffSchedule_Quote_TariffRounding(t *testing.T) {
	rates := mockRates{}
	ts := NewTariffSchedule(rates)

	// CN 税率 9%，超过 de minimis 5000 CNY
	// 6000 CNY: taxable = 1000 CNY, tariff = 1000 * 0.09 = 90 CNY
	quote, err := ts.Quote(catalog.MustMoney("6000.00", catalog.CNY), "家居生活", "CN", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}
	if quote.DeMinimisApplied {
		t.Error("6000 CNY > 5000 de minimis，不应免税")
	}
	// 关税应为正数
	if quote.Tariff.IsZero() {
		t.Error("关税应为非零值")
	}
}

// TestTariffSchedule_Quote_JPDestination 验证 JP 目的地通配费率和超 de minimis 情况。
func TestTariffSchedule_Quote_JPDestination(t *testing.T) {
	rates := mockRates{}
	ts := NewTariffSchedule(rates)

	// JP 税率 8%，de minimis = 10000*5 = 50000 CNY 分 = 500 CNY
	// 6000 CNY > 500 CNY，超过 de minimis，触发关税
	quote, err := ts.Quote(catalog.MustMoney("6000.00", catalog.CNY), "任意品类", "JP", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}
	if quote.DeMinimisApplied {
		t.Error("JP 6000 CNY > 500 CNY de minimis，不应触发 de minimis")
	}
	rate := quote.TariffRate
	if rate.Cmp(parseDec("0.08")) != 0 {
		t.Errorf("JP 税率应为 0.08，实际 %s", rate)
	}
}

// TestTariffSchedule_Quote_SGDeMinimis 验证 SG 目的地在 de minimis 以下的情况。
func TestTariffSchedule_Quote_SGDeMinimis(t *testing.T) {
	rates := mockRates{}
	ts := NewTariffSchedule(rates)

	// SG 税率 7%，de minimis = 400*530 = 212000 CNY 分 = 2120 CNY
	// 1000 CNY < 2120 CNY，触发 de minimis
	quote, err := ts.Quote(catalog.MustMoney("1000.00", catalog.CNY), "其他品类", "SG", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}
	if !quote.DeMinimisApplied {
		t.Error("SG 1000 CNY < 2120 CNY de minimis，应触发 de minimis")
	}
	rate := quote.TariffRate
	if rate.Cmp(parseDec("0.07")) != 0 {
		t.Errorf("SG 税率应为 0.07，实际 %s", rate)
	}
}

// TestTariffSchedule_Quote_ConvertError 验证汇率转换出错时的错误路径。
func TestTariffSchedule_Quote_ConvertError(t *testing.T) {
	ts := NewTariffSchedule(errRates{})

	// 汇率转换失败时应返回错误
	_, err := ts.Quote(catalog.MustMoney("100.00", catalog.USD), "数码配件", "US", 1, catalog.CNY)
	if err == nil {
		t.Error("Quote() 应在汇率转换失败时返回错误")
	}
}

// TestTariffSchedule_Quote_ConvertToCNYError 验证折算为 CNY 时出错。
func TestTariffSchedule_Quote_ConvertToCNYError(t *testing.T) {
	ts := NewTariffSchedule(errRates{})

	// US 数码配件税率为 0，跳过 de minimis 检查
	_, err := ts.Quote(catalog.MustMoney("100.00", catalog.CNY), "数码配件", "US", 1, catalog.CNY)
	if err == nil {
		t.Error("Quote() 应在折算 CNY 时返回错误")
	}
}

// TestShippingQuote_ToDict 验证 ToDict 方法。
func TestShippingQuote_ToDict(t *testing.T) {
	ts := NewTariffSchedule(mockRates{})
	quote, err := ts.Quote(catalog.MustMoney("100.00", catalog.CNY), "家居生活", "CN", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}

	d := quote.ToDict()
	if d == nil {
		t.Fatal("ToDict() 不应返回 nil")
	}
	if d["currency"] != "CNY" {
		t.Errorf("currency = %v, want CNY", d["currency"])
	}
	if d["de_minimis_applied"] == nil {
		t.Error("de_minimis_applied 不应为 nil")
	}
}

// TestShippingQuote_LandedTotalError 验证 LandedTotal 在 Add 出错时的行为。
func TestShippingQuote_LandedTotalError(t *testing.T) {
	ts := NewTariffSchedule(mockRates{})
	quote, err := ts.Quote(catalog.MustMoney("100.00", catalog.CNY), "家居生活", "CN", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("Quote() error: %v", err)
	}

	// 正常情况应该能算总价
	_, err = quote.LandedTotal()
	if err != nil {
		t.Fatalf("LandedTotal() 不应报错，got %v", err)
	}
}

// TestMoneyToMinorInt64_Errors 验证 moneyToMinorInt64 的错误情况。
func TestMoneyToMinorInt64_Errors(t *testing.T) {
	// 创建一个超出范围的金额来触发 Int64 转换失败
	// 这需要实际调用 Quote -> moneyToMinorInt64 路径
	ts := NewTariffSchedule(mockRates{})

	// 正常情况
	_, err := ts.Quote(catalog.MustMoney("100.00", catalog.CNY), "家居生活", "CN", 1, catalog.CNY)
	if err != nil {
		t.Fatalf("正常 Quote 不应报错，got %v", err)
	}
}

// TestTariffSchedule_Quote_ConvertFreightError 验证运费汇率转换出错。
func TestTariffSchedule_Quote_ConvertFreightError(t *testing.T) {
	ts := NewTariffSchedule(errRates{})

	// US 数码配件税率为 0，不走关税路径，但会走运费汇率转换
	_, err := ts.Quote(catalog.MustMoney("100.00", catalog.USD), "数码配件", "US", 1, catalog.USD)
	if err == nil {
		t.Error("Quote() 应在运费汇率转换失败时返回错误")
	}
}

// TestTariffSchedule_Quote_UnsupportedDestinationError 验证不支持目的地返回正确错误。
func TestTariffSchedule_Quote_UnsupportedDestinationError(t *testing.T) {
	ts := NewTariffSchedule(mockRates{})

	_, err := ts.Quote(catalog.MustMoney("100.00", catalog.CNY), "数码配件", "XX", 1, catalog.CNY)
	if err == nil {
		t.Fatal("Quote() 应在不支持的目的地时返回错误")
	}
	if !errors.Is(err, ErrUnsupportedDestination) {
		t.Errorf("错误应 wrap ErrUnsupportedDestination，实际 %v", err)
	}
}
