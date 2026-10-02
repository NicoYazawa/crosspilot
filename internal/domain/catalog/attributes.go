package catalog

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// 这一文件解析的是 catalog.products.attributes 这一列里 `skus` 数组的形状。
//
// 为什么放在领域层：它是商品目录自己的存储约定，两个读取方（按 ID 读取单件
// 商品的适配器、批量列目录的适配器）必须对它有**同一套**理解。各写一份的
// 后果不是重复代码，而是两份逐渐分叉的规则——同一行数据在一处能读出规格、
// 在另一处读不出，且都自认为正确。

// AttributesFromJSON 解析 attributes 列。
//
// 空值返回 nil map 而不是错误：这一列可以为空，不算畸形。
func AttributesFromJSON(data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return nil, nil
	}
	var attrs map[string]any
	if err := json.Unmarshal(data, &attrs); err != nil {
		return nil, fmt.Errorf("解析商品属性失败: %w", err)
	}
	return attrs, nil
}

// SKUsFromAttributes 从 attributes 的 skus 数组还原规格明细。
//
// 两种「没有」被刻意分开：
//
//   - 没有 skus 键、或数组为空 → 返回 nil, nil，表示「这件商品没有规格明细」，
//     由调用方决定退回单规格还是跳过；
//   - skus 存在但形状不对 → 返回错误。把它也当成「没有规格」会把一行损坏的
//     数据静默降级成一个看起来正常的单规格商品。
//
// 单个规格的元素坏掉（缺 sku_id、价格解不出）同样返回错误而不是跳过：数组中
// 少一个规格意味着这件商品少一个可售规格，无声地少卖比报错更难发现。
func SKUsFromAttributes(attrs map[string]any, fallbackCurrency Currency) ([]SKU, error) {
	raw, ok := attrs["skus"]
	if !ok {
		return nil, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("attributes.skus 不是数组，实际为 %T", raw)
	}
	if len(items) == 0 {
		return nil, nil
	}

	out := make([]SKU, 0, len(items))
	for i, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("attributes.skus[%d] 不是对象，实际为 %T", i, item)
		}

		id, _ := m["sku_id"].(string)
		if id == "" {
			return nil, fmt.Errorf("attributes.skus[%d] 缺少 sku_id", i)
		}

		// 规格自带币种；缺失时退回商品的主币种，而不是默认成某种币。
		cur := fallbackCurrency
		if c, _ := m["currency"].(string); c != "" {
			parsed, err := ParseCurrency(c)
			if err != nil {
				return nil, fmt.Errorf("attributes.skus[%d] 的币种 %q 非法: %w", i, c, err)
			}
			cur = parsed
		}

		price, err := MoneyFromJSON(m["price_major"], cur)
		if err != nil {
			return nil, fmt.Errorf("attributes.skus[%d]（%s）的价格无法解析: %w", i, id, err)
		}

		spec, _ := m["spec"].(string)
		stock, _ := m["stock"].(float64)
		out = append(out, SKU{
			ID:    id,
			Spec:  spec,
			Price: price,
			Stock: int(stock),
		})
	}
	return out, nil
}

// MoneyFromJSON 把 JSON 里的价格数字转成领域金额。
//
// float64 是 JSONB 解出来最常见的形式，用最短表示（'f', -1）格式化再交给
// decimal 解析：导入时写进 attributes 的是价格字面量，这样来回一趟形状不变，
// 不会把 199.0 变成 199.00000000000003。
func MoneyFromJSON(v any, cur Currency) (Money, error) {
	switch n := v.(type) {
	case float64:
		return ParseMoney(strconv.FormatFloat(n, 'f', -1, 64), cur)
	case json.Number:
		return ParseMoney(n.String(), cur)
	case string:
		return ParseMoney(n, cur)
	case nil:
		return Money{}, fmt.Errorf("价格字段缺失")
	default:
		return Money{}, fmt.Errorf("价格字段类型不支持: %T", v)
	}
}
