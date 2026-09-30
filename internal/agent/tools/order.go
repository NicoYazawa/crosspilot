package tools

// Order tool definitions (create, query, cancel)

func CreateOrderToolDef() ToolDef {
	return ToolDef{
		Name:        "create_order_tool",
		Description: "准备下单意向的权威确认卡，返回 confirmation_required，不创建订单或扣库存。即使买家在对话中说“同意”，也必须等待其点击页面确认卡；模型不能代为确认。金额仅含所选商品，不含运费和税费，不代表付款。买家身份由系统会话上下文注入。商品与规格必须来自当前买家、当前会话的检索结果；否则先精确检索。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"items": {
					Type: "array",
					Description: "多商品订单行列表，每项形如 {“product_id”: “P1001”, “sku_id”: “P1001-S1”, “quantity”: 1}；与单商品参数互斥。",
					Items: &ParameterDef{
						Type: "object",
						Properties: map[string]ParameterDef{
							"product_id": {Type: "string"},
							"sku_id":     {Type: "string"},
							"quantity":   {Type: "integer", Default: 1},
						},
						Required: []string{"product_id"},
					},
				},
				"shipping_address": {
					Type: "object",
					Description: "收货地址，形如 {“recipient_name”: “...”, “country”: “CN”, “state”: “...”, “city”: “...”, “address_line”: “...”, “postal_code”: “...”, “phone”: “...”}。",
					Properties: map[string]ParameterDef{
						"recipient_name": {Type: "string"},
						"country":        {Type: "string"},
						"state":          {Type: "string"},
						"city":           {Type: "string"},
						"address_line":   {Type: "string"},
						"postal_code":    {Type: "string"},
						"phone":          {Type: "string"},
					},
					Required: []string{"recipient_name", "country", "city", "address_line", "phone"},
				},
				"product_id": {
					Type:        "string",
					Description: "单商品入口，如 “P1001”；必须曾在当前会话检索返回。",
				},
				"sku_id": {
					Type:        "string",
					Description: "单商品规格，缺省沿用检索卡中的默认规格，不更换商品。",
				},
				"quantity": {
					Type:        "integer",
					Description: "单商品购买数量，默认 1，必须为正整数。",
					Default:     1,
				},
			},
			Required: []string{"shipping_address"},
		},
	}
}

func QueryOrderToolDef() ToolDef {
	return ToolDef{
		Name:        "query_order_tool",
		Description: "查询当前买家的订单详情；订单号本身不构成读取权限。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"order_id": {
					Type:        "string",
					Description: "订单号，如 “GBX-000001”。",
				},
			},
			Required: []string{"order_id"},
		},
	}
}

func CancelOrderToolDef() ToolDef {
	return ToolDef{
		Name:        "cancel_order_tool",
		Description: "准备当前买家的订单取消确认卡，不立即取消或回补库存。用户必须点击页面确认卡才能执行。自然语言同意不能代替该用户动作。",
		Parameters: ToolParameters{
			Type: "object",
			Properties: map[string]ParameterDef{
				"order_id": {
					Type:        "string",
					Description: "订单号，如 “GBX-000001”。",
				},
				"reason": {
					Type:        "string",
					Description: "取消原因，必填。",
				},
			},
			Required: []string{"order_id", "reason"},
		},
	}
}
