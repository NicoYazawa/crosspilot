package pricing

import (
	"strings"
	"testing"
)

// priceBookFor 造一张小表，用来隔离 Price 的各条分支。
func priceBookFor(t *testing.T, yml string) *PriceBook {
	t.Helper()
	book, err := LoadFromReader(strings.NewReader(yml), "test://price")
	assertNoError(t, "LoadFromReader", err)
	return book
}

const qwenBook = `
version: "t"
entries:
  - {provider: qwen, model: qwen3-max, kind: input,        unit_minor: "2",   currency: CNY}
  - {provider: qwen, model: qwen3-max, kind: output,       unit_minor: "10",  currency: CNY}
  - {provider: qwen, model: qwen3-max, kind: cached_input, unit_minor: "0.5", currency: CNY}
`

// TestPrice_基本换算 用「最小单位 = 1e-6 元」的直接后果：unit_minor 就等于
// 厂商的「元 / 百万 token」，于是 100 万 input token 恰好值 2 元 = 2_000_000 单位。
func TestPrice_基本换算(t *testing.T) {
	t.Parallel()
	book := priceBookFor(t, qwenBook)

	q, err := book.Price("qwen", "qwen3-max", 1_000_000, 0, 0)
	assertNoError(t, "Price", err)
	assertFalse(t, "百万 input 不该是 unpriced", q.Unpriced)
	equal(t, "100 万 input 的费用", q.CostMinor, int64(2_000_000))
	equal(t, "币种", q.Currency, "CNY")
}

// TestPrice_缓存token不重复计费 是本包最要紧的一条口径。
//
// OpenAI 兼容族的 prompt_tokens 是**总量**，cached_tokens 是其中命中前缀缓存的
// 那部分。若按 input × 单价 + cached × 单价 两笔相加，命中的那批 token 被收了
// 两次钱——成本凭空翻倍，而且从表里完全看不出来。
func TestPrice_缓存token不重复计费(t *testing.T) {
	t.Parallel()
	book := priceBookFor(t, qwenBook)

	// 1000 input 中有 400 命中缓存：全价 600 × 2 + 缓存 400 × 0.5 = 1200 + 200 = 1400
	q, err := book.Price("qwen", "qwen3-max", 1000, 0, 400)
	assertNoError(t, "Price", err)
	assertFalse(t, "不该是 unpriced", q.Unpriced)
	equal(t, "含缓存的费用", q.CostMinor, int64(1400))
	// 反证：若把 input 全额计价再加缓存，会得到 1000*2 + 400*0.5 = 2200。
	if q.CostMinor == 2200 {
		t.Fatal("缓存 token 被重复计费了")
	}
}

// TestPrice_推理token不重复计费 断言 reasoning 只落库留痕、不进费用。
//
// completion_tokens 是输出总量，reasoning_tokens 是其中一个**子集**
// （思维链模型的思维内容）。再乘一次输出单价就是双重收费。
func TestPrice_推理token不重复计费(t *testing.T) {
	t.Parallel()
	book := priceBookFor(t, qwenBook)

	// Price 的入参里根本没有 reasoning 维度——这正是设计：reasoning 含在
	// output 里，没有单独的计费项可加。这里断言 output 只被计一次。
	q, err := book.Price("qwen", "qwen3-max", 0, 100, 0)
	assertNoError(t, "Price", err)
	equal(t, "100 output 的费用", q.CostMinor, int64(1000))
}

// TestPrice_用量为零的维度缺价不算unpriced 断言判据是「有量无价」而非「缺条目」。
//
// 一个没有缓存命中的模型（价目表可以干脆不写 cached_input），不应该因为
// 缺这一条就被判成「不知道花了多少钱」——那会把正常的已定价调用淹掉。
func TestPrice_用量为零的维度缺价不算unpriced(t *testing.T) {
	t.Parallel()
	// 故意不给 cached_input 价目
	book := priceBookFor(t, `
version: "t"
entries:
  - {provider: qwen, model: qwen3-max, kind: input,  unit_minor: "2", currency: CNY}
  - {provider: qwen, model: qwen3-max, kind: output, unit_minor: "10", currency: CNY}
`)

	q, err := book.Price("qwen", "qwen3-max", 100, 10, 0)
	assertNoError(t, "Price", err)
	assertFalse(t, "没有缓存命中的调用不该因缺 cached 价目而 unpriced", q.Unpriced)
	equal(t, "费用", q.CostMinor, int64(300))
}

// TestPrice_有量无价必须unpriced 是 F4 闸门的核心断言。
//
// 「未命中价格表」与「这次调用不花钱」是两件事。把前者记成 0 元，成本看板会
// 长期低报，且从数字上完全看不出异常。
func TestPrice_有量无价必须unpriced(t *testing.T) {
	t.Parallel()
	book := priceBookFor(t, `
version: "t"
entries:
  - {provider: qwen, model: qwen3-max, kind: input,  unit_minor: "2", currency: CNY}
  - {provider: qwen, model: qwen3-max, kind: output, unit_minor: "10", currency: CNY}
`)

	// 本次有 500 个缓存命中 token，但表里没有 cached_input 价目
	q, err := book.Price("qwen", "qwen3-max", 1000, 10, 500)
	assertNoError(t, "Price", err)
	assertTrue(t, "有缓存量却无缓存价，必须标 unpriced", q.Unpriced)
	equal(t, "unpriced 时费用必须是 0 而不是半截值", q.CostMinor, int64(0))
}

// TestPrice_未知model为unpriced 断言名字对不上时同样标 unpriced。
func TestPrice_未知model为unpriced(t *testing.T) {
	t.Parallel()
	book := priceBookFor(t, qwenBook)

	q, err := book.Price("qwen", "qwen3-max-preview", 100, 10, 0)
	assertNoError(t, "Price", err)
	assertTrue(t, "未知 model 必须 unpriced", q.Unpriced)
	equal(t, "未定价费用", q.CostMinor, int64(0))
	// 币种仍要给出确定值：observability.cost.currency 是 NOT NULL
	equal(t, "兜底币种", q.Currency, "CNY")
}

// TestPrice_全零用量是合法的0元 断言「确实没有 token」与「不知道用量」不同。
//
// 上游明确返回了 usage 但各维都是 0，是一次真实的、不花钱的调用。
func TestPrice_全零用量是合法的0元(t *testing.T) {
	t.Parallel()
	book := priceBookFor(t, qwenBook)

	q, err := book.Price("qwen", "qwen3-max", 0, 0, 0)
	assertNoError(t, "Price", err)
	assertFalse(t, "全零用量是已定价的 0 元", q.Unpriced)
	equal(t, "费用", q.CostMinor, int64(0))
}

// TestPrice_负缓存量被钳制 断言上游口径异常时不会反向冲抵别的维度。
//
// 若 cached > input，全价部分会变成负数，乘上单价后从总额里**减掉**一笔钱，
// 算出一个偏低且看不出异常的数字。钳到 0，宁可少算这一维。
func TestPrice_负缓存量被钳制(t *testing.T) {
	t.Parallel()
	book := priceBookFor(t, qwenBook)

	// input=100, cached=500（上游口径异常）→ 全价部分钳到 0；缓存按 500 计
	q, err := book.Price("qwen", "qwen3-max", 100, 0, 500)
	assertNoError(t, "Price", err)
	assertFalse(t, "仍应可定价", q.Unpriced)
	equal(t, "全价部分不得为负", q.CostMinor, int64(250)) // 500 * 0.5
}

// TestPrice_币种随条目 断言按 (provider, model) 取币种，而非全局默认。
//
// Anthropic 按美元报价。若把它的价目按人民币落库，看板上会凭空多出一笔
// 约 7 倍的成本。
func TestPrice_币种随条目(t *testing.T) {
	t.Parallel()
	book := priceBookFor(t, `
version: "t"
entries:
  - {provider: anthropic, model: claude-sonnet-5-5, kind: input,  unit_minor: "2", currency: USD}
  - {provider: anthropic, model: claude-sonnet-5-5, kind: output, unit_minor: "10", currency: USD}
`)

	q, err := book.Price("anthropic", "claude-sonnet-5-5", 1000, 0, 0)
	assertNoError(t, "Price", err)
	equal(t, "币种随条目", q.Currency, "USD")
}

// TestPrice_NilBook 断言空表不 panic 且一律 unpriced。
func TestPrice_NilBook(t *testing.T) {
	t.Parallel()
	var book *PriceBook

	q, err := book.Price("qwen", "qwen3-max", 100, 10, 0)
	assertNoError(t, "nil PriceBook 不该报错", err)
	assertTrue(t, "nil PriceBook 必须 unpriced", q.Unpriced)
}

// TestPrice_小数单价不丢零头 断言在 decimal 域内累加完再取整。
//
// 逐维取整会把 0.5 之类的零头逐笔丢掉：单次调用看着只差 1 个单位，
// 累计到看板上就是一条系统性偏低、永远对不上账单的曲线。
func TestPrice_小数单价不丢零头(t *testing.T) {
	t.Parallel()
	book := priceBookFor(t, `
version: "t"
entries:
  - {provider: qwen, model: qwen3-max, kind: input,  unit_minor: "0.5", currency: CNY}
  - {provider: qwen, model: qwen3-max, kind: output, unit_minor: "0.5", currency: CNY}
`)

	// 两维各 3 个 token、单价 0.5 → 各 1.5，合计 3.0
	// 若逐维取整：1 + 1 = 2，丢掉 1。
	q, err := book.Price("qwen", "qwen3-max", 3, 3, 0)
	assertNoError(t, "Price", err)
	equal(t, "零头应保留到最后一次性取整", q.CostMinor, int64(3))
}

// ---- Default：内嵌价格表 ---------------------------------------------------

// TestDefault_内嵌表可加载 断言 go:embed 的表真的编进了二进制且能解析。
//
// 这条测试防的是「文件没进镜像」的隐蔽失败：外挂 YAML 缺失时服务照常启动、
// /health 照常 200，只是全量 unpriced。内嵌把这个失败模式挪到了编译期，
// 但前提是有人真的断言它。
func TestDefault_内嵌表可加载(t *testing.T) {
	t.Parallel()
	book, err := Default()
	assertNoError(t, "Default", err)
	assertTrue(t, "内嵌表不该为空", book.Size() > 0)
	equal(t, "来源标记", book.Source, "embedded:default_pricing.yaml")
}

// TestDefault_覆盖四家provider 断言四家在用的组合都在表里。
//
// 这是与装配默认值的耦合断言：LLM 的默认 model 名若与价格表分叉，
// 线上会静默变成全量 unpriced。分叉时这条测试会红，逼着两边一起改。
func TestDefault_覆盖四家provider(t *testing.T) {
	t.Parallel()
	book, err := Default()
	assertNoError(t, "Default", err)

	cases := []struct{ provider, model, currency string }{
		{"deepseek", "deepseek-flash", "CNY"},
		{"qwen", "qwen3-max", "CNY"},
		{"minimax", "MiniMax-M2", "CNY"},
		{"anthropic", "claude-sonnet-5-5", "USD"},
	}
	for _, c := range cases {
		assertTrue(t, c.provider+"/"+c.model+" 应在内嵌表中", book.HasModel(c.provider, c.model))
		q, err := book.Price(c.provider, c.model, 1_000_000, 1_000_000, 0)
		assertNoError(t, c.provider+" Price", err)
		assertFalse(t, c.provider+" 百万进百万出不该 unpriced", q.Unpriced)
		equal(t, c.provider+" 币种", q.Currency, c.currency)
		assertTrue(t, c.provider+" 费用应为正", q.CostMinor > 0)
	}
}

// TestDefault_一次真实下单查询的量级 断言成本粒度选对了。
//
// 这是「最小单位 = 1e-6 元」这个决策的回归测试：同样一次调用若按「分」计，
// 结果是 0——那正是成本看板长期显示「没花钱」的成因。
func TestDefault_一次真实下单查询的量级(t *testing.T) {
	t.Parallel()
	book, err := Default()
	assertNoError(t, "Default", err)

	// 实测量级：一次「登山包」查询约 3600 input + 800 output
	q, err := book.Price("deepseek", "deepseek-flash", 3600, 800, 0)
	assertNoError(t, "Price", err)
	assertFalse(t, "不该 unpriced", q.Unpriced)
	// 3600*2 + 800*8 = 7200 + 6400 = 13600 最小单位 = 0.0136 元
	equal(t, "费用", q.CostMinor, int64(13600))
	assertTrue(t, "按分计会得到 0，本口径必须为正", q.CostMinor > 0)
}
