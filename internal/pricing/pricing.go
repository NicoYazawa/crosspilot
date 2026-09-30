// Package pricing 是版本化的 token 价格表。
//
// 价格表由外部 YAML 加载（路径走配置），价格表不存在时返回空表；
// 所有调用走 Lookup，未命中一律返回 ok=false，调用方把这次调用标 unpriced。
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

const (
	KindInput     TokenKind = "input"
	KindOutput    TokenKind = "output"
	KindCached    TokenKind = "cached_input"
	KindReasoning TokenKind = "reasoning"
)

// Entry 是 (provider, model, kind) 的单价。
//
// UnitCost 是「每 1 token 多少分」——价格表里不用浮点。
// Currency 是该条价格对应的币种；不同 provider 可以用不同币种，
// 汇总时由调用方做汇率换算（本包不内置汇率）。
type Entry struct {
	Provider Provider
	Model    string
	Kind     TokenKind
	UnitCost decimal.Decimal // 每 1 token 多少"分"
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
//	  - {provider: qwen, model: qwen3-max, kind: input,    unit_minor: 4,    currency: CNY}
//	  - {provider: qwen, model: qwen3-max, kind: output,   unit_minor: 12,   currency: CNY}
//
// unit_minor 的语义：每 1 token 多少「分」（最小货币单位）。
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
	defer f.Close()
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
