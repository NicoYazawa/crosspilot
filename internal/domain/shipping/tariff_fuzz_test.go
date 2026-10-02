package shipping

import (
	"errors"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
)

// fuzzTargetCurrencies 是报价目标币种池，覆盖两位/零位/三位小数位。
var fuzzTargetCurrencies = []catalog.Currency{catalog.CNY, catalog.USD, catalog.JPY, catalog.KWD}

// identityRates 是模糊测试用汇率表：同币种原样返回，跨币种按 1:1 换算。
// 与 mockRates 不同，它用 NewMoney 而非 MustMoney，非法币种或溢出输入返回错误
// 而不是 panic，因此可安全地接受任意 fuzz 输入。
type identityRates struct{}

func (identityRates) Convert(m catalog.Money, target catalog.Currency) (catalog.Money, error) {
	if m.Currency == target {
		return m, nil
	}
	return catalog.NewMoney(m.Amount, target)
}

// FuzzTariffScheduleQuote 验证一次成功的报价满足领域不变量：
//   - 三要素（小计/运费/关税）都落在目标币种；
//   - 应用的税率必须等于该目的地/品类在费率表中登记的费率，未登记则走 "*" 兜底；
//   - 零税率意味着零关税且不涉及 de minimis；
//   - 标记了 de minimis 则关税必为零；
//   - 到手总价等价于手工累加三要素：同成功且相等，或同因溢出报错。
//
// 入参非法（数量非正、目的地不支持、金额畸形、币种非法）时 Quote 应返回错误，
// 此时直接结束本轮，不再断言成功态不变量。
func FuzzTariffScheduleQuote(f *testing.F) {
	// 种子覆盖：正常路径、低于/高于 de minimis、多件运费、负数金额、
	// 巨额金额、空品类、非法数量、不支持目的地、畸形金额。
	f.Add("100.00", "家居生活", "CN", 1, 0)
	f.Add("0.00", "数码配件", "US", 1, 0)
	f.Add("5000.00", "旅行装备", "US", 1, 0)
	f.Add("6000.00", "家居生活", "CN", 3, 0)
	f.Add("1000.00", "未知品类XYZ", "SG", 1, 0)
	f.Add("-5.00", "家居生活", "CN", 1, 0)
	f.Add("99999999999999.99", "任意品类", "JP", 1, 0)
	f.Add("10.00", "", "EU", 1, 0)
	f.Add("10.00", "数码配件", "CN", 0, 0)
	f.Add("10.00", "数码配件", "CN", -1, 0)
	f.Add("10.00", "数码配件", "XX", 1, 0)
	f.Add("not-a-number", "家居生活", "CN", 1, 0)
	f.Add("10.00", "数码配件", "CN", 1, 3)
	// 小计逼近可表示上限、运费再叠加时到手总价会溢出——这是边界而非缺陷，
	// 因为 LandedTotal 的契约就是「溢出返回错误」。
	f.Add("99999999999998700", "0", "JP", 60, 0)

	f.Fuzz(func(t *testing.T, subtotal, category, shipTo string, quantity, curIdx int) {
		cur := fuzzTargetCurrencies[absMod(curIdx, len(fuzzTargetCurrencies))]

		sub, err := catalog.ParseMoney(subtotal, catalog.CNY)
		if err != nil {
			return // 畸形金额：本轮无成功态可断言
		}

		ts := NewTariffSchedule(identityRates{})
		q, err := ts.Quote(sub, category, shipTo, quantity, cur)
		if err != nil {
			return // 非法数量/不支持目的地/换算失败均为预期错误路径
		}

		// 三要素币种必须都等于目标币种。
		if q.Subtotal.Currency != cur || q.Freight.Currency != cur || q.Tariff.Currency != cur {
			t.Errorf("报价币种不一致: subtotal=%s freight=%s tariff=%s，期望 %s",
				q.Subtotal.Currency, q.Freight.Currency, q.Tariff.Currency, cur)
		}

		// 应用的税率必须等于费率表登记值（未列品类走 "*"）。
		wantRate, ok := _tariffRates[shipTo][category]
		if !ok {
			wantRate = _tariffRates[shipTo]["*"]
		}
		if q.TariffRate.Cmp(wantRate) != 0 {
			t.Errorf("目的地 %s 品类 %q 的税率 = %s，期望 %s", shipTo, category, q.TariffRate, wantRate)
		}

		// 零税率 ⇒ 零关税且不涉及 de minimis（代码注释承诺的语义）。
		if q.TariffRate.IsZero() && (!q.Tariff.IsZero() || q.DeMinimisApplied) {
			t.Errorf("零税率却产生关税或 de minimis: tariff=%s deMinimis=%v", q.Tariff, q.DeMinimisApplied)
		}
		// de minimis 豁免 ⇒ 关税必为零。
		if q.DeMinimisApplied && !q.Tariff.IsZero() {
			t.Errorf("de minimis 豁免下关税应为零，实际 %s", q.Tariff)
		}

		// 到手总价必须等价于手工累加「小计 + 运费 + 关税」：LandedTotal 的契约是
		// 溢出即返回错误，所以成功态要求两者同成功且相等，失败态要求同样溢出。
		// 注意：报价三要素各自可表示，但其和仍可能超出上限，此时报错是合法的。
		total, totalErr := q.LandedTotal()
		want, wantErr := q.Subtotal.Add(q.Freight)
		if wantErr == nil {
			want, wantErr = want.Add(q.Tariff)
		}
		if (totalErr == nil) != (wantErr == nil) {
			t.Fatalf("LandedTotal 与手工累加成败不一致: totalErr=%v wantErr=%v", totalErr, wantErr)
		}
		if totalErr != nil {
			if !errors.Is(totalErr, catalog.ErrAmountOverflow) {
				t.Errorf("LandedTotal 溢出时应返回 ErrAmountOverflow，实际 %v", totalErr)
			}
			return
		}
		if !total.Equal(want) {
			t.Errorf("LandedTotal = %s，期望 %s", total, want)
		}
	})
}

// absMod 返回 n % m 的非负余数，避免 fuzz 传入负索引时越界。
func absMod(n, m int) int {
	r := n % m
	if r < 0 {
		r += m
	}
	return r
}
