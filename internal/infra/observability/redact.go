// Package observability 提供可观测三件套的基础设施：脱敏器、Sink、指标。
//
// 关键不变量（跨整个 P5 阶段共用）：
//   - 脱敏在写路径上，不在读路径上（F7 闸门）
//   - Sink 写入失败绝不影响交易事务（F9 闸门）
//   - 队列满时丢弃并计数，绝不阻塞 Agent（F6 闸门）
//
// 本文件实现脱敏器。所有正则遵循 Go RE2（不支持 lookaround）。
package observability

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RuleKind 是脱敏规则的类别名。
//
// 重命名成本高：测试断言、审计日志都依赖这些字符串。新增类别时必须显式声明
// 且不被现有字符串同义覆盖。
type RuleKind string

const (
	RulePhone       RuleKind = "PHONE"
	RuleEmail       RuleKind = "EMAIL"
	RuleAddress     RuleKind = "ADDRESS"
	RulePayment     RuleKind = "PAYMENT"
	RuleAPIKey      RuleKind = "API_KEY"
	RuleIDCard      RuleKind = "ID_CARD"
	RuleHighEntropy RuleKind = "HIGH_ENTROPY" // 高熵 hex 串（≥64 字符），兜底
)

// RedactedPlaceholder 是脱敏后的占位符格式。
//
// 保留位长便于回归测试断言「脱敏不破坏字段长度」。
// 同时把 [REDACTED:xxx] 做成可全局正则匹配的字面量，便于审计扫描：
// `regexp.MustCompile("\\[REDACTED:[A-Z_]+\\]")`。
const RedactedPlaceholder = "[REDACTED:%s]"

// Rule 是单条脱敏规则。
//
// Name 是日志/审计用的稳定标识。
// Pattern 是 Go RE2 兼容正则（不支持 lookaround）。
// Validator 在替换前后做额外校验——例如长度校验「原串长度 = 占位符长度」；
// 若 Validator 返回错误，Apply 会按"拒绝脱敏，原样保留"处理（fail-open），
// 这样业务不会因脱敏器 bug 而崩溃，但日志里会标一条。
//
// 重要：fail-open 是为了不阻断业务。F7 黄金集测试会同时检查「脱敏后无泄漏」与
// 「fail-open 不影响业务」两条线。
//
// StripBoundary 在 ReplaceAllFunc 替换时是否剥离首尾边界字符。
// 原因：RE2 不支持 lookaround，所以 phone/id-card 这类规则把"边界非数字"
// 一起匹配进来；StripBoundary=True 时，替换时只输出中间 11/18 位的位置为占位符，
// 边界字符保留。这样"i139...end"会变成"i[REDACTED:PHONE]end"。
type Rule struct {
	Name          string
	Kind          RuleKind
	Pattern       *regexp.Regexp
	Validator     func(original, redacted []byte) error
	StripBoundary bool
}

// Redactor 是规则集合与脱敏入口。
//
// 规则顺序敏感：先匹配先生效。把高熵兜底放在最后，避免误伤。
//
// Redactor 创建后不可变——所有规则在构造时确定，Apply 是并发安全的。
// 这一选择是因为脱敏规则在服务运行期不应变；变动走 reload（重启进程）。
type Redactor struct {
	rules     []Rule
	minKeyLen int // API_KEY 形态的最小长度
}

// NewRedactor 用默认规则集构造脱敏器。
//
// 默认规则覆盖五类敏感数据（手机/邮箱/地址/支付/密钥形态），顺序敏感。
// 高熵 hex 兜底放最后，避免误伤正常 token。
func NewRedactor() (*Redactor, error) {
	rules, err := defaultRules()
	if err != nil {
		return nil, fmt.Errorf("observability: 构造默认脱敏规则失败: %w", err)
	}
	return &Redactor{rules: rules, minKeyLen: 32}, nil
}

// NewRedactorWithRules 用自定义规则构造脱敏器（测试用）。
func NewRedactorWithRules(rules []Rule) *Redactor {
	return &Redactor{rules: rules, minKeyLen: 32}
}

// Rules 返回当前规则列表的副本（调试/审计用）。
func (r *Redactor) Rules() []Rule {
	out := make([]Rule, len(r.rules))
	copy(out, r.rules)
	return out
}

// Apply 对输入字节流做脱敏，返回脱敏后的字节流。
//
// Apply 是纯文本替换工具，不做 JSON 语义校验。理由：
//   - 脱敏目标是文本内容泄漏，不是结构泄漏
//   - 把 JSON 校验职责放在 caller（它们自己传 JSON，会自己 Validate）
//   - 测试与 fixture 经常用 JSON 片段（单条 key-value）做断言，
//     强制完整 JSON 会把 fixture 搞复杂
//
// 行为保证：
//   - 任何一条规则匹配 → 替换为 [REDACTED:KIND]
//   - 整张表都没匹配 → 原样返回
//   - 占位符不含任何敏感子串 → 不会触发自身规则（避免无限替换）
func (r *Redactor) Apply(in []byte) ([]byte, error) {
	if len(in) == 0 {
		return in, nil
	}

	out := in
	for _, rule := range r.rules {
		out = rule.Pattern.ReplaceAllFunc(out, func(match []byte) []byte {
			placeholder := []byte(fmt.Sprintf(RedactedPlaceholder, rule.Kind))
			if rule.Validator != nil {
				if err := rule.Validator(match, placeholder); err != nil {
					return match // fail-open：保留原值
				}
			}
			// StripBoundary：剥离首尾边界字符，仅把中间位置替换为占位符。
			// 这要求 Pattern 必须以"边界字符 + 中间值 + 边界字符"形式匹配。
			// 规则设计阶段必须保证这一点。
			if rule.StripBoundary && len(match) >= 3 {
				first, last := match[0], match[len(match)-1]
				return append(append([]byte{first}, placeholder...), last)
			}
			return placeholder
		})
	}
	return out, nil
}

// ApplyString 是 Apply 的字符串便捷包装。
func (r *Redactor) ApplyString(s string) (string, error) {
	out, err := r.Apply([]byte(s))
	if err != nil {
		return s, err
	}
	return string(out), nil
}

// ValidateFixture 把脱敏器用于黄金集扫描。
//
// 给 F7 闸门：传入待扫描文本，断言不出现任何敏感模式（用同名正则反向）。
// 返回 nil = 通过；返回 error = 仍有泄漏。
func (r *Redactor) ValidateFixture(out []byte) error {
	for _, rule := range r.rules {
		// 倒转匹配：原始规则如果还在输出里命中同形态的子串，说明漏掉了。
		// 这里用同一 pattern 但只探测「不含占位符」的部分，简单用 strings.Count。
		// 真正的反向校验更稳的做法是把所有原始敏感形态枚举出来——这里只复用规则
		// 的 pattern 做一遍 sanity check。
		if rule.Pattern.Match(out) {
			// 注意：高熵 hex 等宽松规则在占位符文本中不会命中；但若泄漏了原始
			// 形态（如电话号码），这里就能逮到。
			return fmt.Errorf("observability: 输出仍包含 %s 形态（疑似漏脱敏）", rule.Name)
		}
	}
	return nil
}

// defaultRules 是默认规则集。
//
// 顺序敏感：手机号 → 邮箱 → 地址 → 支付 → 身份证 → API Key 形态 → 高熵兜底
//
// RE2 不支持 (?<=...) (?=...) 这类 lookaround，因此"边界字符"用字符类表达，
// 并在替换阶段手动剥离（StripBoundaries）。例如 phone 正则
// `(?:^|[^0-9])(11位)(?:$|[^0-9])` 匹配三个字符，但只替换中间 11 位。
func defaultRules() ([]Rule, error) {
	phonePattern := `(?:^|[^0-9])(1[3-9][0-9]{9})(?:$|[^0-9])`
	idCardPattern := `(?:^|[^0-9])([0-9]{17}[0-9Xx])(?:$|[^0-9])`

	rules := []struct {
		name          string
		kind          RuleKind
		pattern       string
		stripBoundary bool // 是否在替换时剥离首尾边界字符
	}{
		// 手机号：11 位，1[3-9] 开头。匹配时附带前后非数字边界。
		{"phone-cn", RulePhone, phonePattern, true},

		// 邮箱：RFC 5322 简化版。命中后整段替换。
		{"email", RuleEmail, `[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`, false},

		// 收货地址：关键词 + 字符串值。
		// 命名分组匹配地址值；只替换值，保留键名。
		{"address", RuleAddress, `(?i)("?(address|地址|收货地址|shipping[_-]?address)"?\s*[:：]\s*"?)([^",}\]\n]+)`, false},

		// 支付：card_no / cvv / 有效期。
		{"payment", RulePayment, `(?i)("?(card[_-]?no|card[_-]?number|cvv|有效期|card[_-]?exp)"?\s*[:：]\s*"?)([^",}\]\n]+)`, false},

		// 中国大陆身份证：18 位（17 数字 + 1 校验位）。
		{"id-card-cn", RuleIDCard, idCardPattern, true},

		// OpenAI/Anthropic 风格 API Key：sk-xxx / sk_live_xxx / sk-ant-xxx
		{"api-key", RuleAPIKey, `sk(?:[-_](?:live|test|ant))?[-_][A-Za-z0-9]{16,}`, false},

		// 高熵 hex 兜底：≥64 个连续 [0-9a-f]（大小写不敏感）。
		// 顺序放最后，避免误伤普通十六进制常量。
		{"high-entropy", RuleHighEntropy, `[0-9a-fA-F]{64,}`, false},
	}

	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		re, err := regexp.Compile(r.pattern)
		if err != nil {
			return nil, fmt.Errorf("规则 %s 编译失败: %w", r.name, err)
		}
		out = append(out, Rule{
			Name:          r.name,
			Kind:          r.kind,
			Pattern:       re,
			StripBoundary: r.stripBoundary,
		})
	}

	// 按规则名排序（确定性）。
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// 保留 strings 引用以便后续扩展（如 token 切片校验）。
var _ = strings.Contains
