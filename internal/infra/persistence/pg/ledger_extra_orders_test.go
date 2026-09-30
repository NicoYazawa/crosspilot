package pg_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	"github.com/NicoYazawa/crosspilot/internal/domain/trade"
)

// 本文件覆盖 Store.Orders 与 Store.Order 两条读取路径。
//
// 它们此前没有任何断言，却是买家最常走的路径：排序或计数错了，用户看到的是
// 「订单少了一笔」；归属过滤错了，用户看到的是别人的订单。
// 文件顶部的四个辅助函数被本目录其它 ledger_extra_* 用例共用。

// ledgerExtraCreateRequest 构造一份指定买家与会话的下单请求。
//
// 复用 createRequest 的商品与地址，只替换身份并重算有效期。
// 有效期必须晚于当前假时钟：这些用例会用 Advance 制造不同的 created_at，
// 沿用 createRequest 固定的 5 分钟窗口会在时钟推进后变成「已过期」，
// 那样测到的就不再是排序或分页，而是过期判定。
func ledgerExtraCreateRequest(env *testEnv, operationID, buyerID, sessionID string, items ...trade.Item) trade.PrepareRequest {
	if len(items) == 0 {
		items = []trade.Item{defaultItem(1)}
	}
	req := createRequest(operationID, items...)
	req.BuyerID = buyerID
	req.SessionID = sessionID
	req.ExpiresAt = env.clock.Now().Add(time.Hour)
	return req
}

// ledgerExtraCreateOrder 走账本真实路径建一笔已确认订单并返回订单快照。
//
// 刻意不直接 INSERT orders：这里要验证的是「账本写下的订单能被正确读回」，
// 绕过写入路径造数据，读路径的断言就失去了意义。
func ledgerExtraCreateOrder(t *testing.T, env *testEnv, operationID, buyerID, sessionID string, items ...trade.Item) trade.OrderSnapshot {
	t.Helper()

	confirmation := env.prepare(t, ledgerExtraCreateRequest(env, operationID, buyerID, sessionID, items...))
	confirmed := env.resolve(t, confirmation, trade.DecisionApprove)
	if confirmed.Result == nil {
		t.Fatalf("批准下单后应带订单结果：%+v", confirmed)
	}
	return *confirmed.Result
}

// ledgerExtraCancelRequest 构造一份取消请求。
//
// 有效期同样取当前假时钟之后：取消用例通常发生在时钟推进之后，
// 若沿用固定窗口，失败原因会变成「确认已过期」而不是被测的那条规则。
func ledgerExtraCancelRequest(env *testEnv, operationID, buyerID, sessionID, orderID, reason string) trade.PrepareRequest {
	return trade.PrepareRequest{
		OperationID: operationID,
		BuyerID:     buyerID,
		SessionID:   sessionID,
		Action:      trade.ActionCancel,
		OrderID:     orderID,
		Reason:      reason,
		ExpiresAt:   env.clock.Now().Add(time.Hour),
	}
}

// ledgerExtraOrderIDs 取出列表里的订单号，便于逐项比较顺序。
func ledgerExtraOrderIDs(page trade.OrderPage) []string {
	out := make([]string, 0, len(page.Orders))
	// 用下标索引而不是值拷贝：单个快照元素就有 200 字节
	for i := range page.Orders {
		out = append(out, page.Orders[i].OrderID)
	}
	return out
}

// TestLedgerExtraOrdersOrderingAndPaging 钉住订单列表的排序与分页契约。
//
// 排序是分页正确性的前提：顺序若不稳定，同一页在两次请求之间就会出现重复或遗漏。
// 这里同时覆盖两级排序键——created_at DESC（新订单在前）与 order_id DESC
// （同一时刻创建的订单也必须有确定顺序），因此最后一笔订单刻意不推进时钟，
// 让它与前一笔的 created_at 完全相同。
func TestLedgerExtraOrdersOrderingAndPaging(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 10, 12900, catalog.CNY)
	ctx := context.Background()

	created := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		if i > 0 && i < 4 {
			env.clock.Advance(time.Second)
		}
		snapshot := ledgerExtraCreateOrder(t, env, fmt.Sprintf("operation-order-%d", i), "buyer-1", "session-1")
		created = append(created, snapshot.OrderID)
	}

	// created[3] 与 created[4] 的 created_at 相同，顺序只能由 order_id DESC 决定，
	// 因此期望顺序在这里是唯一确定的，而不是「碰巧」与随机订单号同序。
	higher, lower := created[3], created[4]
	if higher < lower {
		higher, lower = lower, higher
	}
	want := []string{higher, lower, created[2], created[1], created[0]}

	page, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-1", Limit: 10})
	if err != nil {
		t.Fatalf("列出订单失败：%v", err)
	}
	if got := ledgerExtraOrderIDs(page); !slices.Equal(got, want) {
		t.Errorf("订单顺序 = %v，期望 %v", got, want)
	}
	if page.Total != 5 {
		t.Errorf("Total = %d，期望 5", page.Total)
	}
	if page.Offset != 0 || page.Limit != 10 {
		t.Errorf("回显分页参数 = (offset %d, limit %d)，期望 (0, 10)", page.Offset, page.Limit)
	}

	t.Run("Limit 只约束本页条数", func(t *testing.T) {
		bounded, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-1", Limit: 2})
		if err != nil {
			t.Fatalf("列出订单失败：%v", err)
		}
		if got := ledgerExtraOrderIDs(bounded); !slices.Equal(got, want[:2]) {
			t.Errorf("订单 = %v，期望前两条 %v", got, want[:2])
		}
		// Total 是过滤后的全量，与 Limit 无关：分页控件靠它算总页数，
		// 若跟着 Limit 一起缩水，前端就会以为永远只有一页。
		if bounded.Total != 5 {
			t.Errorf("Total = %d，期望 5（与 Limit 无关）", bounded.Total)
		}
	})

	t.Run("Offset 与 Limit 组合取最后一页", func(t *testing.T) {
		last, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-1", Offset: 4, Limit: 2})
		if err != nil {
			t.Fatalf("列出订单失败：%v", err)
		}
		if got := ledgerExtraOrderIDs(last); !slices.Equal(got, want[4:]) {
			t.Errorf("订单 = %v，期望 %v", got, want[4:])
		}
		if last.Total != 5 {
			t.Errorf("Total = %d，期望 5（计数不受 Offset 影响）", last.Total)
		}
	})

	t.Run("Offset 越过末尾返回空页而不是错误", func(t *testing.T) {
		beyond, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-1", Offset: 99, Limit: 10})
		if err != nil {
			t.Fatalf("越界翻页不应报错：%v", err)
		}
		if len(beyond.Orders) != 0 {
			t.Errorf("订单数 = %d，期望 0", len(beyond.Orders))
		}
		// 可以断言非 nil：实现把 Orders 初始化成空切片（trade_store.go 的
		// `page := trade.OrderPage{Orders: []trade.OrderSnapshot{}, ...}`），
		// 序列化成 JSON 时是 []，而 nil 会变成 null，前端就得为两种情况分别解析。
		if beyond.Orders == nil {
			t.Error("空页应当是长度 0 的非 nil 切片，而不是 nil")
		}
		if beyond.Total != 5 {
			t.Errorf("Total = %d，期望 5", beyond.Total)
		}
	})
}

// TestLedgerExtraOrdersRejectsInvalidFilters 覆盖列表参数校验。
//
// 这些参数直接来自 HTTP 查询串，越界值必须被明确拒绝而不是被静默夹紧：
// 夹紧要一个「limit=0」变成「limit=1」，调用方会以为自己拿到的是空页。
func TestLedgerExtraOrdersRejectsInvalidFilters(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	cases := []struct {
		name   string
		filter trade.OrderFilter
	}{
		{"Offset 为负", trade.OrderFilter{BuyerID: "buyer-1", Offset: -1, Limit: 10}},
		{"Limit 为零", trade.OrderFilter{BuyerID: "buyer-1", Limit: 0}},
		{"Limit 超过上限", trade.OrderFilter{BuyerID: "buyer-1", Limit: 101}},
		{"状态取值未知", trade.OrderFilter{BuyerID: "buyer-1", Limit: 10, Status: order.Status("MAYBE")}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.store.Orders(ctx, tc.filter)
			assertCode(t, err, trade.CodeInvalidQuery)
		})
	}

	// 边界内的取值必须被接受，否则「拒绝非法参数」会连合法请求一起拒掉
	for _, limit := range []int{1, 100} {
		if _, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-1", Limit: limit}); err != nil {
			t.Errorf("limit=%d 在契约内，不应被拒绝，实际：%v", limit, err)
		}
	}
	// 零值 Status 表示「不过滤」，不能与「未知状态」混为一谈
	if _, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-1", Limit: 10, Status: ""}); err != nil {
		t.Errorf("空状态表示不过滤，不应被拒绝，实际：%v", err)
	}
}

// TestLedgerExtraOrdersScopeBuyerAndFilterStatus 覆盖列表的两条过滤条件。
//
// 「只返回当前买家的订单」是访问控制而不是展示偏好：这里同时断言
// 别家买家自己的列表里确实有那一笔，否则「没出现」有可能只是数据不存在。
func TestLedgerExtraOrdersScopeBuyerAndFilterStatus(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 10, 12900, catalog.CNY)
	ctx := context.Background()

	confirmed := ledgerExtraCreateOrder(t, env, "operation-confirmed", "buyer-1", "session-1")
	// 被取消的订单创建得更晚，因此取消它之后，buyer-1 的列表里它仍排在最前
	env.clock.Advance(time.Second)
	toCancel := ledgerExtraCreateOrder(t, env, "operation-to-cancel", "buyer-1", "session-1")
	env.clock.Advance(time.Second)
	otherBuyer := ledgerExtraCreateOrder(t, env, "operation-other-buyer", "buyer-2", "session-2")

	cancellation := env.prepare(t, ledgerExtraCancelRequest(env,
		"operation-cancel", "buyer-1", "session-1", toCancel.OrderID, "不想要了"))
	env.resolve(t, cancellation, trade.DecisionApprove)

	all, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-1", Limit: 10})
	if err != nil {
		t.Fatalf("列出订单失败：%v", err)
	}
	if all.Total != 2 {
		t.Errorf("Total = %d，期望 2（别家买家的订单不计入）", all.Total)
	}
	if got, want := ledgerExtraOrderIDs(all), []string{toCancel.OrderID, confirmed.OrderID}; !slices.Equal(got, want) {
		t.Errorf("订单顺序 = %v，期望 %v", got, want)
	}

	onlyConfirmed, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-1", Limit: 10, Status: order.StatusConfirmed})
	if err != nil {
		t.Fatalf("按状态过滤失败：%v", err)
	}
	if onlyConfirmed.Total != 1 || !slices.Equal(ledgerExtraOrderIDs(onlyConfirmed), []string{confirmed.OrderID}) {
		t.Errorf("CONFIRMED 订单 = %+v，期望只有 %s", ledgerExtraOrderIDs(onlyConfirmed), confirmed.OrderID)
	}

	onlyCancelled, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-1", Limit: 10, Status: order.StatusCancelled})
	if err != nil {
		t.Fatalf("按状态过滤失败：%v", err)
	}
	if onlyCancelled.Total != 1 || !slices.Equal(ledgerExtraOrderIDs(onlyCancelled), []string{toCancel.OrderID}) {
		t.Errorf("CANCELLED 订单 = %+v，期望只有 %s", ledgerExtraOrderIDs(onlyCancelled), toCancel.OrderID)
	}

	// 别家买家自己的列表里必须有那一笔：这才说明上面的「没出现」来自过滤条件，
	// 而不是因为这笔订单压根没写进去
	foreign, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-2", Limit: 10})
	if err != nil {
		t.Fatalf("列出别家买家订单失败：%v", err)
	}
	if foreign.Total != 1 || !slices.Equal(ledgerExtraOrderIDs(foreign), []string{otherBuyer.OrderID}) {
		t.Errorf("buyer-2 订单 = %+v，期望只有 %s", ledgerExtraOrderIDs(foreign), otherBuyer.OrderID)
	}
	// 三个买家组合里不存在的人必须拿到空页
	empty, err := env.store.Orders(ctx, trade.OrderFilter{BuyerID: "buyer-3", Limit: 10})
	if err != nil {
		t.Fatalf("列出不存在买家的订单不应报错：%v", err)
	}
	if empty.Total != 0 || len(empty.Orders) != 0 {
		t.Errorf("不存在买家的订单 = %+v，期望空页", ledgerExtraOrderIDs(empty))
	}
}

// TestLedgerExtraOrderReadContract 覆盖单笔订单读取的拒绝分支与快照内容。
func TestLedgerExtraOrderReadContract(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	ctx := context.Background()

	confirmed := ledgerExtraCreateOrder(t, env, "operation-1", "buyer-1", "session-1", defaultItem(2))

	t.Run("快照内容与订单行自洽", func(t *testing.T) {
		snapshot, err := env.store.Order(ctx, confirmed.OrderID, "buyer-1")
		if err != nil {
			t.Fatalf("读取订单失败：%v", err)
		}
		if snapshot.Status != order.StatusConfirmed {
			t.Errorf("状态 = %s，期望 CONFIRMED", snapshot.Status)
		}
		if snapshot.Currency != catalog.CNY {
			t.Errorf("币种 = %s，期望 CNY", snapshot.Currency)
		}
		// 最小单位是权威值：2 × 12900 = 25800
		if snapshot.TotalAmountMinor != 25800 {
			t.Errorf("总额最小单位 = %d，期望 25800", snapshot.TotalAmountMinor)
		}
		// 可展示金额必须由同一个字段派生，而不是另算一遍
		if snapshot.TotalAmountMajor != "258.00" {
			t.Errorf("总额 = %q，期望 %q", snapshot.TotalAmountMajor, "258.00")
		}
		if len(snapshot.Lines) != 1 {
			t.Fatalf("订单行数 = %d，期望 1", len(snapshot.Lines))
		}
		line := snapshot.Lines[0]
		if line.SKUID != "sku-1" || line.Quantity != 2 || line.UnitPriceMajor != "129.00" || line.Currency != catalog.CNY {
			t.Errorf("订单行 = %+v，期望 sku-1 × 2 单价 129.00 CNY", line)
		}
		// 逐行小计与总额必须能对上：129.00 × 2 = 258.00
		if line.Currency != snapshot.Currency {
			t.Errorf("订单行币种 %s 与订单币种 %s 不一致", line.Currency, snapshot.Currency)
		}

		// 地址在快照里是一行字符串，且与领域层的 OneLine 完全一致：
		// 空字段被跳过、联系人放在括号里，展示层不需要再做拼接
		if got, want := snapshot.ShippingAddress, address().OneLine(); got != want {
			t.Errorf("收货地址 = %q，期望 OneLine 形式 %q", got, want)
		}
		if got, want := snapshot.ShippingAddress, "CN 上海 上海 测试路 1 号 200000（测试买家 13800000000）"; got != want {
			t.Errorf("收货地址 = %q，期望 %q", got, want)
		}
	})

	t.Run("订单不存在返回未找到", func(t *testing.T) {
		_, err := env.store.Order(ctx, "0123456789abcdef0123456789abcdef", "buyer-1")
		assertCode(t, err, trade.CodeNotFound)
	})

	t.Run("其他买家读取返回归属不符", func(t *testing.T) {
		_, err := env.store.Order(ctx, confirmed.OrderID, "buyer-2")
		assertCode(t, err, trade.CodeOwnerMismatch)
	})

	t.Run("只有订单头没有明细返回迁移错误", func(t *testing.T) {
		// 这里故意绕过账本：账本在同一个事务里同时写订单头与订单行，
		// 因此它写不出「有头无行」的订单。而历史迁移或其它写入方可能留下这种行，
		// 读路径必须明确报「需要迁移」，而不是返回一张没有内容的订单。
		const orphanOrderID = "fedcba9876543210fedcba9876543210"
		if _, err := env.pool.Exec(ctx, `
INSERT INTO orders (order_id, buyer_id, status, currency, total_amount_minor,
    shipping_address_json, created_at, confirmed_at)
VALUES ($1, 'buyer-1', 'CONFIRMED', 'CNY', 100, '{}', $2, $2)`,
			orphanOrderID, fixedNow); err != nil {
			t.Fatalf("写入孤立订单头失败：%v", err)
		}

		_, err := env.store.Order(ctx, orphanOrderID, "buyer-1")
		assertCode(t, err, trade.CodeInventoryMigrationRequired)
	})
}
