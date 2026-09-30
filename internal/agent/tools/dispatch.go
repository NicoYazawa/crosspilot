package tools

// TaskDispatchToolDef and ShoppingFormToolDef

func TaskDispatchToolDef() ToolDef {
	return ToolDef{
		Name:        "task_dispatch_tool",
		Description: "调度专家子代理执行子任务，返回子代理的结论。仅当子任务满足“可并行 / 需要上下文隔离 / 内部调用链较深”任一条件时使用；简单的单步工具调用应自己直接调业务工具完成。多个彼此独立的子任务请在同一轮一次性发起多个本工具调用，系统会并发执行。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"subagent_type": {
					Type:        "string",
					Description: "子代理类型：“search_agent”（跨境商品检索专家）或 “trade_agent”（下单交易专家）。",
					Enum:        []string{"search_agent", "trade_agent"},
				},
				"demands": {
					Type:        "string",
					Description: "自包含的自然语言指令，必须包含子代理完成任务所需的全部上下文（买家偏好、预算、product_id/sku_id、收货地址等），子代理看不到主对话历史。",
				},
			},
			Required: []string{"subagent_type", "demands"},
		},
	}
}

func ShoppingFormToolDef() ToolDef {
	return ToolDef{
		Name:        "shopping_form_tool",
		Description: "澄清选购需求：由 Agent 编写本次问题，展示后结束本轮等待买家提交。买家要求用表单补充条件时也调用本工具。只询问影响本次选择的未知条件，不替买家预选答案。不确定时允许留空或提供明确的“不确定”选项。页面只渲染已注册组件，不接受 HTML/JavaScript。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"title": {
					Type:        "string",
					Description: "简短的中文表单标题。",
				},
				"questions": {
					Type:        "array",
					Description: "按展示顺序提供 id、type、label；type 为 text、number、single_select 或 multi_select。选择题提供 options（value、label），可设置 required、help_text、placeholder；数字题可设置 unit、minimum、maximum。",
					Items: &ParameterDef{
						Type: "object",
						Properties: map[string]ParameterDef{
							"id":          {Type: "string"},
							"type":        {Type: "string", Enum: []string{"text", "number", "single_select", "multi_select"}},
							"label":       {Type: "string"},
							"required":    {Type: "boolean", Default: false},
							"help_text":   {Type: "string"},
							"placeholder": {Type: "string"},
							"options": {
								Type: "array",
								Items: &ParameterDef{
									Type: "object",
									Properties: map[string]ParameterDef{
										"value": {Type: "string"},
										"label": {Type: "string"},
									},
									Required: []string{"value", "label"},
								},
							},
							"unit":    {Type: "string"},
							"minimum": {Type: "number"},
							"maximum": {Type: "number"},
						},
						Required: []string{"id", "type", "label"},
					},
				},
				"context": {
					Type:        "string",
					Description: "已知的选购背景，仅用于指代，不作为买家新的答案。",
					Default:     "",
				},
				"description": {
					Type:        "string",
					Description: "向买家说明本次为什么需要补充这些信息。",
					Default:     "",
				},
			},
			Required: []string{"title", "questions"},
		},
	}
}
