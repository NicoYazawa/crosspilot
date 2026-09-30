package tools

// Preference tool definitions (remember, update, forget)

func RememberPreferenceToolDef() ToolDef {
	return ToolDef{
		Name:        "remember_preference_tool",
		Description: "记住买家的一条长期偏好（跨会话生效）。仅在买家表达出稳定偏好时调用，一次性的临时要求（如「这次要军绿色」）不要记。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"kind": {
					Type:        "string",
					Description: "\"like\"（正向偏好）或 \"dislike\"（忌口/黑名单）。",
					Enum:        []string{"like", "dislike"},
				},
				"statement": {
					Type:        "string",
					Description: "一句话偏好陈述，10 字以内最佳，如「不要塑料材质」。",
				},
			},
			Required: []string{"kind", "statement"},
		},
	}
}

func UpdatePreferenceToolDef() ToolDef {
	return ToolDef{
		Name:        "update_preference_tool",
		Description: "买家明确要求长期修改偏好时，原子替换旧偏好。本轮临时例外不要保存。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"previous_statement": {
					Type:        "string",
					Description: "当前 buyer-preferences 中旧偏好的完整原文。",
				},
				"kind": {
					Type:        "string",
					Description: "新偏好类别，like 为喜欢，dislike 为避免。",
					Enum:        []string{"like", "dislike"},
				},
				"statement": {
					Type:        "string",
					Description: "新偏好原文，最多 500 字符。",
				},
				"memory_id": {
					Type:        "string",
					Description: "当前记忆的稳定 ID，必须从最新偏好提示中读取。",
					Default:     "",
				},
				"expected_version": {
					Type:        "integer",
					Description: "当前记忆版本，禁止猜测。",
					Default:     0,
				},
			},
			Required: []string{"previous_statement", "kind", "statement"},
		},
	}
}

func ForgetPreferenceToolDef() ToolDef {
	return ToolDef{
		Name:        "forget_preference_tool",
		Description: "删除买家的一条长期偏好（撤回后不再影响后续推荐）。仅在买家明确表示某条历史偏好不再适用时调用，本轮的一次性例外不要调用。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"statement": {
					Type:        "string",
					Description: "要删除的偏好原文，必须与 buyer-preferences 里那一行的文字完全一致。",
				},
				"memory_id": {
					Type:        "string",
					Description: "最新记忆提示中的稳定 ID。",
					Default:     "",
				},
				"expected_version": {
					Type:        "integer",
					Description: "最新记忆版本，不允许猜测。",
					Default:     0,
				},
			},
			Required: []string{"statement"},
		},
	}
}
