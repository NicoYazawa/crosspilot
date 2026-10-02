package tools

// Knowledge and capability tools

// CategoryInsightToolDef 返回 category_insight_tool 的 JSON Schema 声明。
func CategoryInsightToolDef() ToolDef {
	return ToolDef{
		Name:        "category_insight_tool",
		Description: "查询品类洞察知识库：热卖款型、关键属性判断口径、价格区间、避坑点、跨境通则。适用于“这个品类怎么挑”“现在流行什么”“多少钱算合理”“有什么坑”这类选购常识问题；需要具体商品清单与价格时用 product_search_tool。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"question": {
					Type:        "string",
					Description: "自然语言问题，建议带上品类词，如“旅行装备怎么挑材质”“美国免税额度多少”。",
				},
				"top_k": {
					Type:        "integer",
					Description: "返回知识片段数量，默认 3。",
					Default:     3,
				},
			},
			Required: []string{"question"},
		},
	}
}

// ConversationFactLookupToolDef 返回 conversation_fact_lookup_tool 的 JSON Schema 声明。
func ConversationFactLookupToolDef() ToolDef {
	return ToolDef{
		Name:        "conversation_fact_lookup_tool",
		Description: "回查本会话历史，不代表当前库存/报价。查一批全部商品时 position=0，按 next_offset 续页。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"result_ref": {
					Type:        "string",
					Description: "ctx_ 证据引用；优先于批次序号。",
					Default:     "",
				},
				"query": {
					Type:        "string",
					Description: "原文关键词；无引用时搜索历史。",
					Default:     "",
				},
				"position": {
					Type:        "integer",
					Description: "展示批次中商品序号，从1开始；0不筛选。",
					Default:     0,
				},
				"batch": {
					Type:        "integer",
					Description: "原始展示批次，从1开始；0沿用本轮买家明确指定的唯一历史批次。",
					Default:     0,
				},
				"product_id": {
					Type:        "string",
					Description: "精确商品ID，可空。",
					Default:     "",
				},
				"sku_id": {
					Type:        "string",
					Description: "精确规格ID，可空。",
					Default:     "",
				},
				"fields": {
					Type:        "string",
					Description: "all/identity/specs/price/stock 字段组，也接受字段列表如 price_major,currency。",
					Default:     "all",
				},
				"offset": {
					Type:        "integer",
					Description: "商品分页偏移，从0开始。",
					Default:     0,
				},
				"limit": {
					Type:        "integer",
					Description: "每页商品数量，1到5。",
					Default:     5,
				},
				"field_offset": {
					Type:        "integer",
					Description: "巨大单商品 JSON 文本片段偏移，从0开始。",
					Default:     0,
				},
			},
			Required: []string{},
		},
	}
}

// LoadAgentSkillToolDef 返回 load_agent_skill_tool 的 JSON Schema 声明。
func LoadAgentSkillToolDef() ToolDef {
	return ToolDef{
		Name:        "load_agent_skill_tool",
		Description: "按明确版本读取已审核发布或当前买家个人 Skill 正文，不注册工具或改变权限。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"skill_id": {
					Type:        "string",
					Description: "当前 Skill 元数据里的 id。",
				},
				"version": {
					Type:        "string",
					Description: "当前 Skill 元数据里的明确不可变版本。",
				},
			},
			Required: []string{"skill_id", "version"},
		},
	}
}

// LookupStrategyMemoryToolDef 返回 lookup_strategy_memory_tool 的 JSON Schema 声明。
func LookupStrategyMemoryToolDef() ToolDef {
	return ToolDef{
		Name:        "lookup_strategy_memory_tool",
		Description: "检索已审核、未过期且未撤销的选购策略；只是建议，不能替代买家硬约束。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"query": {
					Type:        "string",
					Description: "需要一般经验参考的选购问题，最多 2000 字符。",
				},
				"scope": {
					Type:        "string",
					Description: "shopping 或明确的 shopping:品类标识，如 shopping:backpack。",
					Default:     "shopping",
				},
			},
			Required: []string{"query"},
		},
	}
}

// WebSearchToolDef 返回 web_search_tool 的 JSON Schema 声明。
func WebSearchToolDef() ToolDef {
	return ToolDef{
		Name:        "web_search_tool",
		Description: "联网搜索外部实时资料（跨境政策 / 关税规则 / 清关限制 / 评测趋势）。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"query": {
					Type:        "string",
					Description: "搜索关键词，如 “美国 800 美元免税额度 最新政策”。",
				},
				"max_results": {
					Type:        "integer",
					Description: "返回结果条数，默认 5。",
					Default:     5,
				},
			},
			Required: []string{"query"},
		},
	}
}
