package trade

import (
	"errors"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
)

// fixtureNow 是所有用例共用的固定时刻。
//
// 用常量时刻而不是 time.Now()：只要有一个断言依赖「当前时间」，测试就会在不同
// 机器、不同运行时刻给出不同结论，而交易账本里最贵的缺陷恰好都藏在时间边界上
// （恰好到期、已决议但早已过期）。
var fixtureNow = time.Date(2026, 9, 9, 8, 0, 0, 0, time.UTC)

// validAddress 返回一份完整合法的收货地址。
// Country 是唯一只看形状不看内容的字段：必须是两位大写字母。
func validAddress() order.Address {
	return order.Address{
		Recipient:  "李四",
		Phone:      "13800000000",
		Country:    "CN",
		Province:   "上海",
		City:       "上海",
		Line1:      "浦东新区世纪大道 1 号",
		PostalCode: "200120",
	}
}

// asciiAddress 返回一份全 ASCII 的合法地址，供手工书写 JSON 的用例使用：
// 转义序列写错会让「摘要应当相等」的断言以难以定位的方式失败。
func asciiAddress() order.Address {
	return order.Address{
		Recipient:  "Li Si",
		Phone:      "13800000000",
		Country:    "CN",
		Province:   "Shanghai",
		City:       "Shanghai",
		Line1:      "No.1 Century Avenue",
		PostalCode: "200120",
	}
}

// sku 构造一行下单明细。标题一律由调用方给出，因为 Prepare 会用权威库存里的标题
// 覆盖它——这正是「展示的商品名必须来自库存」这条规则要被断言的地方。
func sku(productID, skuID, title string, unitPriceMinor int64, currency catalog.Currency, quantity int64) Item {
	return Item{
		ProductID:      productID,
		SKUID:          skuID,
		Title:          title,
		UnitPriceMinor: unitPriceMinor,
		Currency:       currency,
		Quantity:       quantity,
	}
}

// record 构造一行权威库存。
func record(skuID, productID, title string, unitPriceMinor, stock int64, currency catalog.Currency) InventoryRecord {
	return InventoryRecord{
		SKUID:          skuID,
		ProductID:      productID,
		Title:          title,
		Stock:          stock,
		UnitPriceMinor: unitPriceMinor,
		Currency:       currency,
	}
}

// inventoryOf 把若干库存行按 SKU 索引成 Prepare 需要的映射。
func inventoryOf(records ...InventoryRecord) map[string]InventoryRecord {
	out := make(map[string]InventoryRecord, len(records))
	for _, r := range records {
		out[r.SKUID] = r
	}
	return out
}

// validCreateInput 返回一份「除被测字段外全部合法」的下单请求。
// 表驱动用例只改动触发某条分支的那一个字段，其余保持合法，
// 这样断言到的错误码必然由该字段引起，而不是被别的错误掩盖。
func validCreateInput() PrepareInput {
	return PrepareInput{
		OperationID:     "op-1001",
		BuyerID:         "buyer-1",
		SessionID:       "session-1",
		Action:          ActionCreate,
		Items:           []Item{sku("prod-a", "sku-a", "模型写的标题", 2599, catalog.USD, 2)},
		ShippingAddress: validAddress(),
		ExpiresAt:       fixtureNow.Add(15 * time.Minute),
	}
}

// validDeps 返回与 validCreateInput 配套的依赖。
func validDeps() PrepareDeps {
	return PrepareDeps{
		Now: fixtureNow,
		Inventory: inventoryOf(
			record("sku-a", "prod-a", "权威标题 A", 2599, 10, catalog.USD),
		),
	}
}

// createPayload 返回一份内容固定的下单载荷，便于逐字段断言执行结果。
// 金额 2599×2 + 1299 = 6497 写死，而不是用表达式重算，避免把实现的算法抄进期望值。
func createPayload() Payload {
	return Payload{Create: &CreatePayload{
		Items: []Item{
			sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 2),
			sku("prod-b", "sku-b", "水壶", 1299, catalog.USD, 1),
		},
		ShippingAddress:  validAddress(),
		Currency:         catalog.USD,
		TotalAmountMinor: 6497,
	}}
}

// cancelPayload 返回一份内容固定的取消载荷。
func cancelPayload() Payload {
	return Payload{Cancel: &CancelPayload{
		OrderID:          "order-1",
		Reason:           "买错了",
		OrderStatus:      order.StatusConfirmed,
		Items:            []Item{sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 2)},
		ShippingAddress:  validAddress(),
		Currency:         catalog.USD,
		TotalAmountMinor: 5198,
	}}
}

// orderView 返回一份内容固定的历史订单快照。
// 明细故意乱序，用来检验重建载荷时确实按 SKU 升序排列，而不是沿用库里的顺序。
func orderView() OrderView {
	return OrderView{
		OrderID: "order-1",
		BuyerID: "buyer-1",
		Status:  order.StatusConfirmed,
		SKUs:    []string{"sku-b", "sku-a"},
		Items: []Item{
			sku("prod-b", "sku-b", "水壶", 1299, catalog.USD, 1),
			sku("prod-a", "sku-a", "登山包", 2599, catalog.USD, 2),
		},
		ShippingAddress:  validAddress(),
		Currency:         "USD",
		TotalAmountMinor: 6497,
	}
}

// storedConfirmation 按存储层的方式构造一张摘要自洽的确认单：
// SnapshotHash 由身份字段与载荷算出，Resolve 才会继续走到后面的分支。
func storedConfirmation(t *testing.T, payload Payload, action Action, expiresAt time.Time) Confirmation {
	t.Helper()
	c := Confirmation{
		ConfirmationID: "0123456789abcdef0123456789abcdef",
		OperationID:    "op-1001",
		BuyerID:        "buyer-1",
		SessionID:      "session-1",
		Action:         action,
		Payload:        payload,
		ExpiresAt:      expiresAt,
		Status:         StatusPending,
		CreatedAt:      fixtureNow,
	}
	hash, err := c.ComputeSnapshotHash()
	if err != nil {
		t.Fatalf("计算快照摘要失败: %v", err)
	}
	c.SnapshotHash = hash
	return c
}

// resolveInputOf 返回一份除决议本身外全部合法的决议输入。
func resolveInputOf(stored Confirmation, decision Decision) ResolveInput {
	return ResolveInput{
		ConfirmationID: stored.ConfirmationID,
		BuyerID:        stored.BuyerID,
		SessionID:      stored.SessionID,
		SnapshotHash:   stored.SnapshotHash,
		Decision:       decision,
	}
}

// requestHashOf 走完整的 Prepare 拿到规范化之后的请求摘要。
//
// 直接调用 RequestHash 只能验证哈希函数本身；经过 Prepare 才能验证
// 「排序、合并、标题覆盖之后」的摘要——幂等键的正确性恰恰取决于规范化。
func requestHashOf(t *testing.T, input PrepareInput, deps PrepareDeps) string {
	t.Helper()
	out, err := Prepare(input, deps)
	if err != nil {
		t.Fatalf("Prepare 失败: %v", err)
	}
	return out.Confirmation.RequestHash
}

// codeOf 取出稳定的错误码。
//
// 断言错误码而不是错误消息：消息是给人看的、会改；错误码是接口层映射状态码与
// 前端分支的依据，不能改，因此测试必须钉住它。
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("期望返回错误，实际为 nil")
	}
	var storeErr *StoreError
	if !errors.As(err, &storeErr) {
		t.Fatalf("错误 %v 不是 *StoreError，取不出错误码", err)
	}
	return storeErr.Code
}

// wantCode 断言错误码，并顺带检查 CodeInvalidArgument 与 ErrInvalidArgument 的
// errors.Is 关系：调用方靠它把「参数错」与「状态冲突」分开处理，
// 这层关系断了不会有别的用例发现。
func wantCode(t *testing.T, err error, want string) {
	t.Helper()
	if got := codeOf(t, err); got != want {
		t.Fatalf("错误码 = %s，期望 %s（原始错误：%v）", got, want, err)
	}
	if want == CodeInvalidArgument {
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("errors.Is(err, ErrInvalidArgument) 应为 true，实际 false（err=%v）", err)
		}
		return
	}
	if errors.Is(err, ErrInvalidArgument) {
		t.Errorf("错误码 %s 不应被判定为 ErrInvalidArgument（err=%v）", want, err)
	}
}

// mustHash 计算确认单的快照摘要，失败即终止。
func mustHash(t *testing.T, c Confirmation) string {
	t.Helper()
	hash, err := c.ComputeSnapshotHash()
	if err != nil {
		t.Fatalf("计算快照摘要失败: %v", err)
	}
	return hash
}

// isLowerHex 报告字符串是否为指定长度的小写十六进制。
// 摘要与标识的形状是线上契约（前端会按长度与字符集做校验），必须逐字符检查。
func isLowerHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
