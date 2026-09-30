package trade

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
)

func TestPayloadValidateBranches(t *testing.T) {
	validItem := sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 1)

	cases := []struct {
		name    string
		payload Payload
		// wantCode 为空表示这个载荷必须通过校验。
		wantCode string
	}{
		{"合法的下单载荷", createPayload(), ""},
		{"合法的取消载荷", cancelPayload(), ""},

		// 两个分支同时存在时无法判断该执行哪一种，必须拒绝而不是任选一个。
		{"两个分支同时设置", Payload{Create: createPayload().Create, Cancel: cancelPayload().Cancel}, CodeInvalidArgument},
		{"两个分支都为空", Payload{}, CodeInvalidArgument},

		{"下单载荷没有明细", Payload{Create: &CreatePayload{
			ShippingAddress: validAddress(),
			Currency:        catalog.USD,
		}}, CodeInvalidArgument},
		{"下单载荷地址不完整", Payload{Create: &CreatePayload{
			Items:            []Item{validItem},
			ShippingAddress:  order.Address{Recipient: "李四"},
			Currency:         catalog.USD,
			TotalAmountMinor: 2599,
		}}, CodeInvalidArgument},
		{"下单载荷总额与明细不符", Payload{Create: &CreatePayload{
			Items:            []Item{validItem},
			ShippingAddress:  validAddress(),
			Currency:         catalog.USD,
			TotalAmountMinor: 1,
		}}, CodeInvalidArgument},
		{"下单载荷币种非法", Payload{Create: &CreatePayload{
			Items:            []Item{validItem},
			ShippingAddress:  validAddress(),
			Currency:         "usd",
			TotalAmountMinor: 2599,
		}}, CodeInvalidArgument},

		{"取消载荷缺少订单号", Payload{Cancel: &CancelPayload{
			Reason:           "买错了",
			OrderStatus:      order.StatusConfirmed,
			Items:            []Item{validItem},
			Currency:         catalog.USD,
			TotalAmountMinor: 2599,
		}}, CodeInvalidArgument},
		{"取消载荷原因只有空白", Payload{Cancel: &CancelPayload{
			OrderID:          "order-1",
			Reason:           "   ",
			OrderStatus:      order.StatusConfirmed,
			Items:            []Item{validItem},
			Currency:         catalog.USD,
			TotalAmountMinor: 2599,
		}}, CodeInvalidArgument},
		{"取消载荷订单状态非法", Payload{Cancel: &CancelPayload{
			OrderID:          "order-1",
			Reason:           "买错了",
			OrderStatus:      "BOGUS",
			Items:            []Item{validItem},
			Currency:         catalog.USD,
			TotalAmountMinor: 2599,
		}}, CodeInvalidArgument},
		{"取消载荷没有明细", Payload{Cancel: &CancelPayload{
			OrderID:     "order-1",
			Reason:      "买错了",
			OrderStatus: order.StatusConfirmed,
			Currency:    catalog.USD,
		}}, CodeInvalidArgument},
		{"取消载荷总额与明细不符", Payload{Cancel: &CancelPayload{
			OrderID:          "order-1",
			Reason:           "买错了",
			OrderStatus:      order.StatusConfirmed,
			Items:            []Item{validItem},
			Currency:         catalog.USD,
			TotalAmountMinor: 2,
		}}, CodeInvalidArgument},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.payload.Validate()
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("合法载荷被拒绝: %v", err)
				}
				return
			}
			wantCode(t, err, tc.wantCode)
		})
	}
}

// TestValidateItemsBranches 逐条覆盖明细校验。
//
// 这些判断是「确认单上展示的金额」与「实际执行的金额」是否同一回事的最后一道闸门：
// 漏掉任何一条，买家看到的总额都可能与实际扣款不符。
func TestValidateItemsBranches(t *testing.T) {
	ok := sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 1)

	cases := []struct {
		name     string
		items    []Item
		currency catalog.Currency
		total    int64
		wantCode string
	}{
		{"合法明细", []Item{ok}, catalog.USD, 2599, ""},
		{"多行合法明细", []Item{ok, sku("prod-b", "sku-b", "水壶", 1299, catalog.USD, 2)}, catalog.USD, 5197, ""},

		{"币种非法", []Item{ok}, "usd", 2599, CodeInvalidArgument},
		{"商品标识为空", []Item{sku("  ", "sku-a", "登山包", 2599, catalog.USD, 1)}, catalog.USD, 2599, CodeInvalidArgument},
		{"规格标识为空", []Item{sku("prod-a", "\t", "登山包", 2599, catalog.USD, 1)}, catalog.USD, 2599, CodeInvalidArgument},
		{"数量为零", []Item{sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 0)}, catalog.USD, 0, CodeInvalidArgument},
		{"数量为负", []Item{sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, -1)}, catalog.USD, -2599, CodeInvalidArgument},
		{"单价为负", []Item{sku("prod-a", "sku-a", "登山包", -1, catalog.USD, 1)}, catalog.USD, -1, CodeInvalidArgument},
		{"与载荷币种不一致", []Item{sku("prod-a", "sku-a", "登山包", 2599, catalog.JPY, 1)}, catalog.USD, 2599, CodeInvalidArgument},
		{"单价乘数量溢出", []Item{sku("prod-a", "sku-a", "登山包", 2, catalog.USD, math.MaxInt64)}, catalog.USD, 0, CodeInvalidArgument},
		{"逐行金额之和溢出", []Item{
			sku("prod-a", "sku-a", "登山包", math.MaxInt64-100, catalog.USD, 1),
			sku("prod-b", "sku-b", "水壶", 200, catalog.USD, 1),
		}, catalog.USD, 0, CodeInvalidArgument},
		{"总额与逐行金额之和不一致", []Item{ok}, catalog.USD, 2600, CodeInvalidArgument},
		{"总额为零但明细有金额", []Item{ok}, catalog.USD, 0, CodeInvalidArgument},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateItems(tc.items, tc.currency, tc.total)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("合法明细被拒绝: %v", err)
				}
				return
			}
			wantCode(t, err, tc.wantCode)
		})
	}
}

// TestPayloadMarshalJSONWritesOneBranchOnly 断言线上表示只有一个分支。
// 另一个分支若以 null 混进 JSON，会被 UnmarshalJSON 按 order_id 是否存在误判，
// 也会污染摘要。
func TestPayloadMarshalJSONWritesOneBranchOnly(t *testing.T) {
	createJSON, err := json.Marshal(createPayload())
	if err != nil {
		t.Fatalf("下单载荷序列化失败: %v", err)
	}
	cancelJSON, err := json.Marshal(cancelPayload())
	if err != nil {
		t.Fatalf("取消载荷序列化失败: %v", err)
	}

	// 成员名就是线上契约，前端与既有的评测数据都按它们取值。
	createKeys := []string{"items", "shipping_address", "currency", "total_amount_minor", "amount_scope", "order_kind"}
	for _, key := range createKeys {
		if !bytes.Contains(createJSON, []byte(`"`+key+`"`)) {
			t.Errorf("下单载荷缺少成员 %q：%s", key, createJSON)
		}
	}
	if bytes.Contains(createJSON, []byte(`"order_id"`)) {
		t.Errorf("下单载荷不应带 order_id，否则会被当成取消载荷：%s", createJSON)
	}

	cancelKeys := []string{"order_id", "reason", "order_status", "items", "shipping_address", "currency", "total_amount_minor", "amount_scope", "order_kind"}
	for _, key := range cancelKeys {
		if !bytes.Contains(cancelJSON, []byte(`"`+key+`"`)) {
			t.Errorf("取消载荷缺少成员 %q：%s", key, cancelJSON)
		}
	}

	ambiguous := Payload{Create: createPayload().Create, Cancel: cancelPayload().Cancel}
	if _, err := json.Marshal(ambiguous); codeOf(t, err) != CodeInvalidArgument {
		t.Errorf("两个分支同时设置时序列化应报 %s", CodeInvalidArgument)
	}
	if _, err := json.Marshal(Payload{}); codeOf(t, err) != CodeInvalidArgument {
		t.Errorf("空载荷序列化应报 %s", CodeInvalidArgument)
	}
}

// TestPayloadJSONRoundTripIsByteIdentical 断言往返之后字节完全相同。
//
// 只比较「解析后字段相等」是不够的：字段名一旦变化，反序列化仍然成功，
// 但线上契约已经变了，而摘要（走同一条 JSON 路径）也会跟着变。
func TestPayloadJSONRoundTripIsByteIdentical(t *testing.T) {
	cases := []struct {
		name    string
		payload Payload
	}{
		{"下单载荷", createPayload()},
		{"取消载荷", cancelPayload()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("序列化失败: %v", err)
			}

			var decoded Payload
			if err := json.Unmarshal(first, &decoded); err != nil {
				t.Fatalf("反序列化失败: %v", err)
			}

			second, err := json.Marshal(decoded)
			if err != nil {
				t.Fatalf("再次序列化失败: %v", err)
			}
			if !bytes.Equal(first, second) {
				t.Errorf("往返后字节不同：\n第一次 %s\n第二次 %s", first, second)
			}
		})
	}
}

// TestPayloadUnmarshalDistinguishesBranches 断言两种载荷靠 order_id 区分，
// 且绝不会互相认错：认错的下场是拿一份下单载荷去执行取消。
func TestPayloadUnmarshalDistinguishesBranches(t *testing.T) {
	createWire := `{
		"items": [{"product_id": "prod-a", "sku_id": "sku-a", "title": "登山包", "unit_price_minor": 2599, "currency": "USD", "quantity": 2}],
		"shipping_address": {"recipient": "Li Si", "phone": "13800000000", "country": "CN", "province": "Shanghai", "city": "Shanghai", "line1": "No.1 Road", "line2": "", "postal_code": "200120"},
		"currency": "USD",
		"total_amount_minor": 5198,
		"amount_scope": "merchandise_only",
		"order_kind": "purchase_intent"
	}`
	cancelWire := `{
		"order_id": "order-1",
		"reason": "买错了",
		"order_status": "CONFIRMED",
		"items": [{"product_id": "prod-a", "sku_id": "sku-a", "title": "登山包", "unit_price_minor": 2599, "currency": "USD", "quantity": 2}],
		"shipping_address": {"recipient": "Li Si", "phone": "13800000000", "country": "CN", "province": "Shanghai", "city": "Shanghai", "line1": "No.1 Road", "line2": "", "postal_code": "200120"},
		"currency": "USD",
		"total_amount_minor": 5198,
		"amount_scope": "merchandise_only",
		"order_kind": "purchase_intent"
	}`

	var create Payload
	if err := json.Unmarshal([]byte(createWire), &create); err != nil {
		t.Fatalf("下单载荷解析失败: %v", err)
	}
	if create.Create == nil || create.Cancel != nil {
		t.Fatalf("下单载荷被解析成 %+v", create)
	}
	if got := create.Action(); got != ActionCreate {
		t.Errorf("Action() = %q，期望 %q", got, ActionCreate)
	}
	if got := create.Create.TotalAmountMinor; got != 5198 {
		t.Errorf("总额 = %d，期望 5198", got)
	}
	if got := create.Create.Items[0].Title; got != "登山包" {
		t.Errorf("标题 = %q，期望 登山包", got)
	}
	if got := create.Create.ShippingAddress.City; got != "Shanghai" {
		t.Errorf("城市 = %q，期望 Shanghai", got)
	}
	if err := create.Validate(); err != nil {
		t.Errorf("解析出的下单载荷应当自洽，得到 %v", err)
	}

	var cancel Payload
	if err := json.Unmarshal([]byte(cancelWire), &cancel); err != nil {
		t.Fatalf("取消载荷解析失败: %v", err)
	}
	if cancel.Cancel == nil || cancel.Create != nil {
		t.Fatalf("取消载荷被解析成 %+v", cancel)
	}
	if got := cancel.Action(); got != ActionCancel {
		t.Errorf("Action() = %q，期望 %q", got, ActionCancel)
	}
	if got := cancel.Cancel.OrderID; got != "order-1" {
		t.Errorf("订单号 = %q，期望 order-1", got)
	}
	if got := cancel.Cancel.OrderStatus; got != order.StatusConfirmed {
		t.Errorf("订单状态 = %q，期望 %q", got, order.StatusConfirmed)
	}
	if err := cancel.Validate(); err != nil {
		t.Errorf("解析出的取消载荷应当自洽，得到 %v", err)
	}
}

func TestPayloadUnmarshalRejectsBadInput(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		// 空载荷一旦被执行，就是一张没有内容的订单，必须在这里就断掉。
		{"null", "null"},
		{"空数组", "[]"},
		{"非空数组", "[1,2]"},
		{"字符串", `"下单"`},
		{"数字", "1"},
		{"取消载荷订单状态非法", `{"order_id":"order-1","order_status":"BOGUS"}`},
		{"取消载荷明细类型错误", `{"order_id":"order-1","items":"不是数组"}`},
		{"下单载荷币种类型错误", `{"currency":123}`},
		{"下单载荷明细类型错误", `{"items":"不是数组"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p Payload
			err := json.Unmarshal([]byte(tc.raw), &p)
			if got := codeOf(t, err); got != CodeInvalidArgument {
				t.Errorf("错误码 = %s，期望 %s", got, CodeInvalidArgument)
			}
		})
	}
}

// TestPayloadUnmarshalEmptyObjectIsRejected 钉住「解析阶段就拒绝空载荷」。
//
// 曾经的取舍是「解析成空的下单载荷、由 Validate 兜底」，但那意味着
// 一段既没有订单号也没有明细的 JSON 会被当成「下单、零个商品」——
// 万一有调用路径忘了校验，写下的就是一张没有内容的订单。
// 空载荷因此在解析层就被拒绝，不再依赖下游是否记得校验。
func TestPayloadUnmarshalEmptyObjectIsRejected(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"空对象", `{}`},
		{"只有登记范围", `{"amount_scope":"merchandise_only"}`},
		{"空明细数组", `{"items":[]}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var p Payload
			err := json.Unmarshal([]byte(tc.raw), &p)
			if got := codeOf(t, err); got != CodeInvalidArgument {
				t.Errorf("错误码 = %s，期望 %s", got, CodeInvalidArgument)
			}
		})
	}
}

func TestPayloadActionAndString(t *testing.T) {
	if got := createPayload().Action(); got != ActionCreate {
		t.Errorf("下单载荷 Action() = %q，期望 %q", got, ActionCreate)
	}
	if got := cancelPayload().Action(); got != ActionCancel {
		t.Errorf("取消载荷 Action() = %q，期望 %q", got, ActionCancel)
	}
	// Action 只看 Cancel 是否为空，因此空载荷被当作下单；
	// 这是可接受的，因为空载荷在执行前必然先被 Validate 拦下。
	if got := (Payload{}).Action(); got != ActionCreate {
		t.Errorf("空载荷 Action() = %q，期望 %q", got, ActionCreate)
	}

	cases := []struct {
		name    string
		payload Payload
		want    string
	}{
		{"下单载荷", createPayload(), "下单确认：2 行，6497 USD"},
		{"取消载荷", cancelPayload(), "取消确认：订单 order-1"},
		{"空载荷", Payload{}, "确认单载荷为空"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.payload.String(); got != tc.want {
				t.Errorf("String() = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// TestPayloadCanonicalShape 覆盖参与哈希的映射。
//
// canonical 走一次 JSON 往返，键名就是线上字段名；金额必须是 json.Number，
// 退化成 float64 会让大额金额在往返中改变取值，而摘要必须精确。
func TestPayloadCanonicalShape(t *testing.T) {
	got := createPayload().canonical()

	wantKeys := []string{"items", "shipping_address", "currency", "total_amount_minor", "amount_scope", "order_kind"}
	if len(got) != len(wantKeys) {
		t.Errorf("规范编码成员数 = %d，期望 %d：%v", len(got), len(wantKeys), got)
	}
	for _, key := range wantKeys {
		if _, ok := got[key]; !ok {
			t.Errorf("规范编码缺少成员 %q", key)
		}
	}
	if _, ok := got["create"]; ok {
		t.Error("规范编码不应出现分支名 create")
	}
	if _, ok := got["cancel"]; ok {
		t.Error("规范编码不应出现分支名 cancel")
	}

	number, ok := got["total_amount_minor"].(json.Number)
	if !ok {
		t.Fatalf("总额类型 = %T，期望 json.Number（浮点会丢精度）", got["total_amount_minor"])
	}
	if number.String() != "6497" {
		t.Errorf("总额 = %s，期望 6497", number)
	}
	if got["amount_scope"] != AmountScope || got["order_kind"] != OrderKind {
		t.Errorf("口径/单据类型 = %v/%v，期望 %q/%q", got["amount_scope"], got["order_kind"], AmountScope, OrderKind)
	}

	// 无法序列化的载荷（两分支同时设置或都为空）必须退化成空映射而不是 panic：
	// 交易路径上的 panic 会连带回滚整个事务。
	if got := (Payload{}).canonical(); len(got) != 0 {
		t.Errorf("空载荷的规范编码 = %v，期望空映射", got)
	}
	ambiguous := Payload{Create: createPayload().Create, Cancel: cancelPayload().Cancel}
	if got := ambiguous.canonical(); len(got) != 0 {
		t.Errorf("双分支载荷的规范编码 = %v，期望空映射", got)
	}
}

func TestItemKeyIsSKU(t *testing.T) {
	item := sku("prod-a", "sku-9", "登山包", 2599, catalog.USD, 1)
	if got := item.Key(); got != "sku-9" {
		t.Errorf("Key() = %q，期望 sku-9（同一 SKU 的多行必须落到同一个键上）", got)
	}
}

// TestItemEqualToIgnoresQuantityOnly 断言「同一 SKU 是否自相矛盾」的判定边界：
// 数量可以不同（会被相加），价格、商品、标题、币种任何一项不同都不行。
func TestItemEqualToIgnoresQuantityOnly(t *testing.T) {
	base := sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 1)

	cases := []struct {
		name string
		item Item
		want bool
	}{
		{"完全相同", sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 1), true},
		{"只有数量不同", sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 7), true},
		{"商品标识不同", sku("prod-b", "sku-a", "登山包", 2599, catalog.USD, 1), false},
		{"规格标识不同", sku("prod-a", "sku-b", "登山包", 2599, catalog.USD, 1), false},
		{"标题不同", sku("prod-a", "sku-a", "水壶", 2599, catalog.USD, 1), false},
		{"单价不同", sku("prod-a", "sku-a", "登山包", 2600, catalog.USD, 1), false},
		{"币种不同", sku("prod-a", "sku-a", "登山包", 2599, catalog.JPY, 1), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := base.equalTo(tc.item); got != tc.want {
				t.Errorf("equalTo(%+v) = %v，期望 %v", tc.item, got, tc.want)
			}
		})
	}
}
