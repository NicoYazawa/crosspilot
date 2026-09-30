// YAML 解码辅助。
//
// 只暴露 decodeYAML 一个内部函数给 pricing.go 使用——避免在 API 层暴露
// 通用 yaml 解码能力。schema 严格的解析仍走 pricing.parseYAML。

package pricing

import (
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// decodeYAML 解码 YAML 到 map[string]any。
//
// 不使用 struct tag 是因为我们只关心「校验每个字段」而不是「绑定到结构体」——
// 校验逻辑全部放在 entryFromMap 里集中处理，避免散落。
func decodeYAML(r io.Reader) (map[string]any, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true) // 严格模式：未知字段直接报错
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("yaml 解码失败: %w", err)
	}
	return out, nil
}
