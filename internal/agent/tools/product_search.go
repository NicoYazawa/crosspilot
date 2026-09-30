package tools

// ProductSearchToolDef returns the JSON Schema for the product search tool.
func ProductSearchToolDef() ToolDef {
	return ToolDef{
		Name:        "product_search_tool",
		Description: "检索跨境商品库（embedding+rerank 二阶段召回），返回 Top-K 商品卡 JSON。传入 ship_to 时商品卡自动内联 landed_price 到手价明细（小计+运费+关税，统一折算 target_currency），无需另行计算单件价格。已知商品或规格时直接传 product_id / sku_id，优先精确查询；两者同时传入时必须属于同一商品，未找到不能用相似商品替换。报价按一件计算。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"normalized_query": {
					Type:        "string",
					Description: "标准化检索词，保留品类词与关键属性词；精确 ID 查询时可以省略。",
				},
				"category": {
					Type:        "string",
					Description: "品类槽位，可选，如\"旅行装备\"、\"数码配件\"。",
				},
				"ship_to": {
					Type:        "string",
					Description: "收货国家二位码，可选，如 \"CN\"、\"US\"；传入后过滤不可送达商品并内联到手价。",
				},
				"top_k": {
					Type:        "integer",
					Description: "返回候选数量，默认 5。",
					Default:     5,
				},
				"price_max_major": {
					Type:        "number",
					Description: "价格上限（target_currency 主单位），买家有预算硬约束时必传。",
				},
				"target_currency": {
					Type:        "string",
					Description: "价格口径币种，默认 \"CNY\"。",
					Default:     "CNY",
				},
				"excluded_material_tags": {
					Type:        "array",
					Description: "材质黑名单，如买家明确不要塑料时传 [\"合成聚合物\"]。",
					Items:       &ParameterDef{Type: "string"},
				},
				"required_material_tags": {
					Type:        "array",
					Description: "材质白名单，如必须是金属时传 [\"金属\"]。",
					Items:       &ParameterDef{Type: "string"},
				},
				"product_id": {
					Type:        "string",
					Description: "精确商品 ID，如 \"P1003\"；与 sku_id 同时提供时必须匹配。",
				},
				"sku_id": {
					Type:        "string",
					Description: "精确规格 ID，如 \"P1003-S1\"；优先于 product_id 和查询文字，保持当前规格。",
				},
			},
			Required: []string{},
		},
	}
}
