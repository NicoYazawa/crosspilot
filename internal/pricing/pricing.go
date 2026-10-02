// Package pricing 是版本化的 token 价格表。
//
// 价格表由外部 YAML 加载（路径走配置），价格表不存在时返回空表；
// 所有调用走 Lookup，未命中一律返回 ok=false，调用方把这次调用标 unpriced。
//
// # 货币单位
//
// 本包所有金额以「最小货币单位」计，最小单位定义为 **1e-6 元**（见 Quote）。
// 之所以不是「分」：一次真实的下单查询（数千 token）总成本只有零点几分，
// 按分取整后每一行都是 0，成本看板会长期低报——那正是 F4 要防的静默失败。
//
// 附带的好处是价格表可以直接照抄厂商价目页：厂商按「元 / 百万 token」报价，
// 而在最小单位 = 1e-6 元下，该数字与「每 token 多少最小单位」逐个相等
// （1 元/百万 token = 1e6 单位 / 1e6 token = 1 单位/token）。
//
// 关键不变量：
//   - 价格表为空不报错，只是「全部 unpriced」——F4「unpriced 显式」闸门
//   - 加载期校验：provider 必须在已知集合（qwen/minimax/deepseek/anthropic）
//   - 价格表版本号必须随每次更新递增——前端看板按版本号区分
package pricing

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/govalues/decimal"
)

// Provider 是受支持的四家供应商。
//
// 改这里 = 改 F10 跨协议族口径。Anthropic 走 Anthropic Messages（system +
// tool 单独计数），其余三家走 OpenAI 兼容（系统提示与工具定义并入 input），
// 因此跨族 cached_input 不可直接比较。
type Provider string

// ProviderQwen 及其后的常量是四家受支持供应商的稳定标识，与配置项、YAML
// 价格表、observability 落库值一一对应，只能新增，不能改名或复用。
const (
	ProviderQwen      Provider = "qwen"
	ProviderMiniMax   Provider = "minimax"
	ProviderDeepSeek  Provider = "deepseek"
	ProviderAnthropic Provider = "anthropic"
)

// AllProviders 是合法 provider 集合。启动期校验用。
func AllProviders() []Provider {
	return []Provider{ProviderQwen, ProviderMiniMax, ProviderDeepSeek, ProviderAnthropic}
}

// IsValidProvider 报告 provider 字符串是否合法。
func IsValidProvider(p string) bool {
	for _, v := range AllProviders() {
		if string(v) == p {
			return true
		}
	}
	return false
}

// TokenKind 是 token 计量的四种维度。
//
// cached_input 与 input 价格通常不一致（DeepSeek 差额最大）；
// reasoning 仅 Claude 等少数模型返回；未命中时一律标 unpriced。
type TokenKind string

// KindInput 及其后的常量是 token 计量的四个维度标识，同样是对外契约
// （价格表 YAML 的 kind 字段、成本落库的计量列），只能新增，不能改名。
const (
	KindInput     TokenKind = "input"
	KindOutput    TokenKind = "output"
	KindCached    TokenKind = "cached_input"
	KindReasoning TokenKind = "reasoning"
)

// Entry 是 (provider, model, kind) 的单价。
//
// UnitCost 是「每 1 token 多少最小货币单位（1e-6 元）」——价格表里不用浮点。
// 于是它恰好等于厂商价目页上的「元 / 百万 token」数字，可直接照抄。
//
// Currency 是该条价格对应的币种；不同 provider 可以用不同币种，
// 汇总时由调用方做汇率换算（本包不内置汇率）。
type Entry struct {
	Provider Provider
	Model    string
	Kind     TokenKind
	UnitCost decimal.Decimal // 每 1 token 多少最小货币单位（1e-6 元）
	Currency string
}

// PriceBook 是不可变的价格表快照。
//
// 加载完成后冻结：Load 返回的 PriceBook 是只读视图，所有 Lookup 调用
// 无锁。这一选择是因为价格表在服务运行期不会变；变动走 reload（重启进程）。
type PriceBook struct {
	Version string // 加载时从 YAML 头部读，默认为 "unknown"
	Source  string // 加载来源（文件路径），便于日志/审计
	entries []Entry
}

// Lookup 返回 (provider, model, kind) 的单价。
//
// 命中：返回 (UnitCost, true)。
// 未命中：返回 (decimal.Zero, false) —— 调用方应把这次调用标 unpriced=true
// 绝不允许「未命中按 0 计」（那是 F4 闸门要防的事）。
func (b *PriceBook) Lookup(provider, model string, kind TokenKind) (decimal.Decimal, bool) {
	if b == nil {
		return decimal.Zero, false
	}
	for _, e := range b.entries {
		if e.Provider == Provider(provider) && e.Model == model && e.Kind == kind {
			return e.UnitCost, true
		}
	}
	return decimal.Zero, false
}

// HasModel 报告是否有任何条目覆盖该 model（不看 kind）。
//
// 给 A/B 报表用：判断「这个 model 是否在任何 arm 中被计入过」。
func (b *PriceBook) HasModel(provider, model string) bool {
	if b == nil {
		return false
	}
	for _, e := range b.entries {
		if e.Provider == Provider(provider) && e.Model == model {
			return true
		}
	}
	return false
}

// Size 返回条目总数；调试/审计用。
func (b *PriceBook) Size() int {
	if b == nil {
		return 0
	}
	return len(b.entries)
}

// defaultCurrency 是价格表没给出币种时的兜底。
//
// observability.cost.currency 是 NOT NULL，必须有一个确定的值落库；
// 空串会让「币种未知」和「币种是空」在表里长得一样。
const defaultCurrency = "CNY"

// Quote 是一次模型调用的计价结果。
type Quote struct {
	// CostMinor 是本次调用的费用，单位是最小货币单位（1e-6 元）。
	// Unpriced 为 true 时恒为 0，理由见 Unpriced。
	CostMinor int64
	Currency  string
	// Unpriced 表示「这次调用有计费量，但价格表没有覆盖」。
	//
	// 读取端（application/observability.CostOfRun）对 unpriced 行**整行**不计入
	// 总额、只把 UnpricedCount 加一。所以这里绝不返回「算了一半」的费用：半截成本
	// 与完整成本在表里看不出区别，只会误导直接读表的 SQL，而读取端本来就会丢弃它。
	Unpriced bool
}

// Price 按 token 用量计算一次模型调用的费用。
//
// 计费口径（两个子集关系把它一次性说清，改之前先读 protocol.Usage 的注释）：
//
//	全价 input 量 = tokensIn - tokensCached   // cached 是 input 的子集，不能收两次钱
//	output 量     = tokensOut                 // reasoning 已含在 output 内，不重复计
//	cached 量     = tokensCached
//
// 判定规则：
//   - 该 model 一条价目都没有（空表 / 名字对不上）→ Unpriced
//   - 某个**用量大于 0** 的维度缺价目 → Unpriced（绝不按 0 计，F4 闸门）
//   - 用量为 0 的维度缺价目 → 不影响：没有缓存命中就不该因缺少 cached 价而被判未定价
//   - 全部维度都算得出来 → 返回费用（用量全为 0 时合法的结果是 0 元，且不是 unpriced）
//
// 返回 error 只用于「算不出来」（decimal 溢出等），与 Unpriced 是两件事：
// 前者是内部异常，后者是价格表未覆盖。调用方对两者都应落一条 unpriced 行，
// 但对 error 还要记日志。
func (b *PriceBook) Price(provider, model string, tokensIn, tokensOut, tokensCached int64) (Quote, error) {
	currency := b.currencyOf(provider, model)

	if !b.HasModel(provider, model) {
		return Quote{Currency: currency, Unpriced: true}, nil
	}

	// 上游口径异常时不让负数冲抵别的维度：那会算出一个比真实更低、且从表面
	// 看不出来的数字。钳到 0，宁可少算这一维。
	billableIn := tokensIn - tokensCached
	if billableIn < 0 {
		billableIn = 0
	}

	need := [3]struct {
		qty  int64
		kind TokenKind
	}{
		{billableIn, KindInput},
		{tokensOut, KindOutput},
		{tokensCached, KindCached},
	}

	total := decimal.Zero
	for _, n := range need {
		if n.qty == 0 {
			continue
		}
		unit, ok := b.Lookup(provider, model, n.kind)
		if !ok {
			return Quote{Currency: currency, Unpriced: true}, nil
		}
		tokens, err := decimal.New(n.qty, 0)
		if err != nil {
			return Quote{}, fmt.Errorf("pricing: token 数 %d 无法表示: %w", n.qty, err)
		}
		part, err := unit.Mul(tokens)
		if err != nil {
			return Quote{}, fmt.Errorf("pricing: 计算 %s 费用失败: %w", n.kind, err)
		}
		total, err = total.Add(part)
		if err != nil {
			return Quote{}, fmt.Errorf("pricing: 累加 %s 费用失败: %w", n.kind, err)
		}
	}

	// 在 decimal 域内累加完再取整：单项费用常常是小数（例如 0.5 最小单位/token），
	// 逐维取整会把零头丢光。
	whole, _, ok := total.Round(0).Int64(0)
	if !ok {
		return Quote{}, fmt.Errorf("pricing: 费用 %s 超出可表示范围", total.String())
	}
	return Quote{CostMinor: whole, Currency: currency}, nil
}

// currencyOf 取该 (provider, model) 的币种；没有价目时返回 defaultCurrency。
//
// 取「首条」而不是逐维度判断：同一个 model 的不同 token 维度币种不一致是配置错误，
// 而不是一种可以逐维表达的状态；entries 在加载时已排序，结果确定。
func (b *PriceBook) currencyOf(provider, model string) string {
	if b == nil {
		return defaultCurrency
	}
	for i := range b.entries {
		e := &b.entries[i]
		if e.Provider == Provider(provider) && e.Model == model && e.Currency != "" {
			return e.Currency
		}
	}
	return defaultCurrency
}

// ErrInvalidPriceEntry 表示 YAML 中某条价格记录不合规。
//
// 启动期遇到此错应当 fail-fast：让运维立即知道价格表坏了，而不是带病上线
// 然后全表 unpriced。
type ErrInvalidPriceEntry struct {
	Index  int
	Reason string
}

func (e *ErrInvalidPriceEntry) Error() string {
	return fmt.Sprintf("pricing: 第 %d 条价格不合规：%s", e.Index, e.Reason)
}

// ErrUnknownProvider 表示 provider 不在已知集合。
var ErrUnknownProvider = errors.New("pricing: 未知 provider")

// LoadFromFile 从 YAML 文件加载价格表。
//
// 文件不存在：返回空 PriceBook + nil（这是 dev 默认）。让 dev 体验顺滑。
// 文件存在但解析失败：返回 ErrInvalidPriceEntry，调用方应让进程退出。
//
// YAML schema：
//
//	version: "2026-09-30"
//	entries:
//	  - {provider: qwen, model: qwen3-max, kind: input,    unit_minor: "0.5", currency: CNY}
//	  - {provider: qwen, model: qwen3-max, kind: output,   unit_minor: "2",   currency: CNY}
//
// unit_minor 的语义：每 1 token 多少最小货币单位（1e-6 元），数值等于厂商公布的
// 「元 / 百万 token」。写成字符串可以保留小数精度（走 decimal.Parse）；
// 整数也可以直接写数字。
func LoadFromFile(path string) (*PriceBook, error) {
	if path == "" {
		return &PriceBook{Version: "empty", Source: ""}, nil
	}
	f, err := os.Open(path) // #nosec G304 -- 配置文件路径来自可信配置
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &PriceBook{Version: "missing", Source: path}, nil
		}
		return nil, fmt.Errorf("pricing: 打开价格表 %q 失败: %w", path, err)
	}
	// 只读文件，关闭失败不影响已解析内容，显式丢弃即可
	defer func() { _ = f.Close() }()
	return parseYAML(f, path)
}

// LoadFromReader 从任意 io.Reader 解析价格表。
//
// 测试用：避免在测试里写临时 YAML 文件。
func LoadFromReader(r io.Reader, source string) (*PriceBook, error) {
	return parseYAML(r, source)
}

func parseYAML(r io.Reader, source string) (*PriceBook, error) {
	// 用 yaml.v3 解析 schema 严格的子集，避免引入 OpenAPI 等重型依赖。
	raw, err := decodeYAML(r)
	if err != nil {
		return nil, fmt.Errorf("pricing: 解析 %q 失败: %w", source, err)
	}

	version, _ := raw["version"].(string)
	rawEntries, _ := raw["entries"].([]any)
	entries := make([]Entry, 0, len(rawEntries))
	for i, raw := range rawEntries {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, &ErrInvalidPriceEntry{Index: i, Reason: "entries 项不是对象"}
		}
		e, err := entryFromMap(m)
		if err != nil {
			return nil, &ErrInvalidPriceEntry{Index: i, Reason: err.Error()}
		}
		entries = append(entries, e)
	}

	// 排序便于二分查找（虽然当前是线性，但有序便于测试断言）。
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Provider != entries[j].Provider {
			return entries[i].Provider < entries[j].Provider
		}
		if entries[i].Model != entries[j].Model {
			return entries[i].Model < entries[j].Model
		}
		return entries[i].Kind < entries[j].Kind
	})

	if version == "" {
		version = "unknown"
	}
	return &PriceBook{Version: version, Source: source, entries: entries}, nil
}

func entryFromMap(m map[string]any) (Entry, error) {
	provider, _ := m["provider"].(string)
	if !IsValidProvider(provider) {
		return Entry{}, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
	model, _ := m["model"].(string)
	if model == "" {
		return Entry{}, fmt.Errorf("model 为空")
	}
	kindStr, _ := m["kind"].(string)
	switch TokenKind(kindStr) {
	case KindInput, KindOutput, KindCached, KindReasoning:
	default:
		return Entry{}, fmt.Errorf("kind %q 不合法（必须是 input/output/cached_input/reasoning）", kindStr)
	}
	// unit_minor 支持两种：整数（最常见） 或字符串（精度更高时）。
	var unit decimal.Decimal
	switch v := m["unit_minor"].(type) {
	case int:
		d, err := decimal.New(int64(v), 0)
		if err != nil {
			return Entry{}, fmt.Errorf("unit_minor 构造失败: %w", err)
		}
		unit = d
	case int64:
		d, err := decimal.New(v, 0)
		if err != nil {
			return Entry{}, fmt.Errorf("unit_minor 构造失败: %w", err)
		}
		unit = d
	case float64:
		// float64 仅做兜底：警告生产不应使用浮点
		d, err := decimal.New(int64(v), 0)
		if err != nil {
			return Entry{}, fmt.Errorf("unit_minor 构造失败: %w", err)
		}
		unit = d
	case string:
		parsed, err := decimal.Parse(v)
		if err != nil {
			return Entry{}, fmt.Errorf("unit_minor 解析失败: %w", err)
		}
		unit = parsed
	default:
		return Entry{}, fmt.Errorf("unit_minor 类型不支持（%T）", m["unit_minor"])
	}
	currency, _ := m["currency"].(string)
	if currency == "" {
		currency = "CNY"
	}
	return Entry{
		Provider: Provider(provider),
		Model:    model,
		Kind:     TokenKind(kindStr),
		UnitCost: unit,
		Currency: currency,
	}, nil
}

// Mutex 是 LoadFromFile 的并发安全保护。
//
// 本类型本身无内部状态变更，但导出函数接收 path 字符串时的并发安全由调用方
// 负责；此处保留占位，便于后续加 hot reload（用 RWMutex 保护 internal cache）。
type Mutex struct {
	sync.Mutex
}
