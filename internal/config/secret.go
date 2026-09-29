package config

import (
	"encoding/json"
	"log/slog"
)

// redacted 是 Secret 对外暴露的全部内容。
const redacted = "[REDACTED]"

// Secret 是一个不应出现在日志、错误信息或序列化结果中的字符串。
//
// 读真实值必须显式调用 Reveal，这样「密钥出现在哪里」在代码里是可检索的：
// 搜索 Reveal 的调用点就能列出所有接触明文的位置。
//
// Secret 实现了 fmt.Stringer、fmt.GoStringer、json.Marshaler 与 slog.LogValuer，
// 因此用 %v、%s、%#v、%+v 打印或直接结构化日志记录都不会泄漏明文。
type Secret string

// Reveal 返回明文。调用点应尽量少，且不得把返回值写入日志或错误信息。
func (s Secret) Reveal() string { return string(s) }

// String 实现 fmt.Stringer，永远返回打码后的固定串。
func (s Secret) String() string { return redacted }

// GoString 实现 fmt.GoStringer，覆盖 %#v 输出。
func (s Secret) GoString() string { return redacted }

// LogValue 实现 slog.LogValuer。
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON 实现 json.Marshaler，永远输出打码后的值。
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// IsZero 报告密钥是否未设置。
func (s Secret) IsZero() bool { return s == "" }
