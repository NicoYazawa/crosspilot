package observability

// Redact 返回只含白名单字段的副本，用于在记录之前收敛可落地的信息。
//
// 这里用白名单而不是黑名单：新增字段默认不记录，忘记登记只会少记，
// 不会把新出现的敏感字段静默写进日志。
//
// 返回的是浅拷贝，嵌套结构仍与入参共享，调用方不应再修改它们。
func Redact(attrs map[string]any, allowlist []string) map[string]any {
	if len(attrs) == 0 || len(allowlist) == 0 {
		return map[string]any{}
	}

	allowed := make(map[string]struct{}, len(allowlist))
	for _, key := range allowlist {
		allowed[key] = struct{}{}
	}

	out := make(map[string]any, len(allowlist))
	for key, value := range attrs {
		if _, ok := allowed[key]; ok {
			out[key] = value
		}
	}
	return out
}

// AnySensitive 报告 attrs 中是否存在非空的敏感字段。
//
// 用于在写入前做一次兜底断言：如果出现了不该出现的东西，宁可整条不记，
// 也不要冒险落盘。
func AnySensitive(attrs map[string]any, sensitive []string) bool {
	for _, key := range sensitive {
		if value, ok := attrs[key]; ok && value != nil {
			return true
		}
	}
	return false
}
