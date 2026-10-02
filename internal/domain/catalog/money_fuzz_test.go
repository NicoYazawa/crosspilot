package catalog

import (
	"errors"
	"testing"
)

// fuzzCurrencies 是 FuzzMoneyParse 使用的币种池，覆盖两位/零位/三位小数位，
// 末位放一个形状非法的币种以走通 ErrInvalidCurrency 分支。
var fuzzCurrencies = []Currency{USD, CNY, JPY, KWD, "usd"}

// FuzzMoneyParse 验证 ParseMoney 的两条核心不变量：
//  1. 确定性——同一输入重复解析必须得到相等的金额；
//  2. 往返一致——解析成功时小数位必须等于币种小数位，且金额经过
//     String() 再解析回来仍相等（Money 的规范表示唯一）。
//
// 失败时返回的错误必须落在文档承诺的 sentinel 集合内，不出现其它错误。
func FuzzMoneyParse(f *testing.F) {
	// 种子覆盖：空串、合法金额、畸形输入、负数、超大值、unicode、各精度边界。
	f.Add("", 0)
	f.Add("0", 0)
	f.Add("29.99", 0)
	f.Add("0.00", 1)
	f.Add("-12.34", 0)
	f.Add("1.5", 0)                 // 需补零
	f.Add("1.005", 0)               // 需舍入
	f.Add("1234", 2)                // 零位币种
	f.Add("1.2345", 3)              // 三位币种
	f.Add("9999999999999999999", 0) // 超出可表示范围
	f.Add("abc", 0)                 // 畸形
	f.Add("1.2.3", 0)               // 畸形
	f.Add("NaN", 0)                 // 非有限
	f.Add("¥29.99", 0)              // unicode
	f.Add("٢٩", 0)                  // 阿拉伯-印度数字
	f.Add("1.00", 4)                // 非法币种

	f.Fuzz(func(t *testing.T, s string, curIdx int) {
		cur := fuzzCurrencies[absMod(curIdx, len(fuzzCurrencies))]

		first, err := ParseMoney(s, cur)
		if err != nil {
			// 失败必须是文档承诺的三类领域错误之一。
			if !errors.Is(err, ErrInvalidAmount) &&
				!errors.Is(err, ErrInvalidCurrency) &&
				!errors.Is(err, ErrAmountOverflow) {
				t.Fatalf("ParseMoney(%q, %s) 返回了未承诺的错误: %v", s, cur, err)
			}
			return
		}

		// 不变量 1：确定性。
		second, err := ParseMoney(s, cur)
		if err != nil {
			t.Fatalf("ParseMoney(%q, %s) 首次成功、二次失败: %v", s, cur, err)
		}
		if !first.Equal(second) {
			t.Errorf("ParseMoney 不确定: %s != %s", first, second)
		}

		// 不变量 2：小数位必须收敛到币种小数位。
		if got, want := first.Amount.Scale(), cur.Scale(); got != want {
			t.Errorf("ParseMoney(%q, %s) 小数位 = %d，期望 %d", s, cur, got, want)
		}

		// 不变量 3：String()/Parse 往返后金额相等。
		round, err := ParseMoney(first.Amount.String(), cur)
		if err != nil {
			t.Fatalf("解析 %s 的规范表示失败: %v", first, err)
		}
		if !round.Equal(first) {
			t.Errorf("往返不一致: %s → %s → %s", s, first.Amount, round.Amount)
		}
	})
}

// FuzzMoneyArithmetic 验证 Money 算术满足代数律：加法交换、减法等价于加上相反数、
// 自比较为零、绝对值非负且与取反的绝对值幂等。溢出属于合法结果，报错即提前返回。
func FuzzMoneyArithmetic(f *testing.F) {
	f.Add("10.50", "2.25")
	f.Add("0", "0")
	f.Add("-12.34", "5.00")
	f.Add("29.99", "0.01")
	f.Add("99999999999999999.99", "99999999999999999.99") // 溢出
	f.Add("abc", "1.00")                                  // 畸形

	f.Fuzz(func(t *testing.T, as, bs string) {
		a, aErr := ParseMoney(as, USD)
		b, bErr := ParseMoney(bs, USD)
		if aErr != nil || bErr != nil {
			return
		}

		// 交换律：(a+b) == (b+a)。
		ab, err := a.Add(b)
		if err != nil {
			return // 溢出是合法结果
		}
		ba, err := b.Add(a)
		if err != nil {
			t.Fatalf("a+b 成功但 b+a 失败: %v", err)
		}
		if !ab.Equal(ba) {
			t.Errorf("加法不满足交换律: %s vs %s", ab, ba)
		}

		// a - b == a + (-b)。
		sub, err := a.Sub(b)
		if err != nil {
			return
		}
		viaNeg, err := a.Add(b.Neg())
		if err != nil {
			t.Fatalf("a-b 成功但 a+(-b) 失败: %v", err)
		}
		if !sub.Equal(viaNeg) {
			t.Errorf("减法与加相反数不一致: %s vs %s", sub, viaNeg)
		}

		// 自比较为零，且 Abs 非负、与 Neg 的绝对值一致。
		if c, err := a.Cmp(a); err != nil || c != 0 { //nolint:gocritic // fuzz 刻意构造 x 与自身比较的边界，验证自比较恒为 0
			t.Errorf("a.Cmp(a) = %d, err=%v，期望 0, nil", c, err)
		}
		if a.Abs().IsNegative() {
			t.Errorf("Abs 不应为负: %s", a.Abs())
		}
		if !a.Abs().Equal(a.Neg().Abs()) {
			t.Errorf("Abs 与 Neg.Abs 不一致: %s vs %s", a.Abs(), a.Neg().Abs())
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
