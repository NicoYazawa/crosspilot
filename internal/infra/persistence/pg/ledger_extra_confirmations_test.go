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

// 本文件覆盖确认单读取、库存读取与「拒绝」决议三条薄路径。
//
// 拒绝是最容易被漏测的一条：它不写订单也不动库存，正因如此，
// 一旦它悄悄改了订单状态或回补了库存，肉眼很难从结果上看出来。

// ledgerExtraConfirmationIDs 取出确认单标识，便于逐项比较顺序。
func ledgerExtraConfirmationIDs(list []trade.Confirmation) []string {
	out := make([]string, 0, len(list))
	// 用下标索引而不是值拷贝：单张确认单就有 208 字节
	for i := range list {
		out = append(out, list[i].ConfirmationID)
	}
	return out
}

// TestLedgerExtraConfirmationLookupUnknownID 覆盖按标识读取确认单的未找到分支。
func TestLedgerExtraConfirmationLookupUnknownID(t *testing.T) {
	env := newEnv(t)

	_, err := env.store.Confirmation(context.Background(),
		"0123456789abcdef0123456789abcdef", "buyer-1", "session-1")
	assertCode(t, err, trade.CodeNotFound)
}

// TestLedgerExtraConfirmationsClampLimitAndScopeToOwner 覆盖确认单列表的三条契约：
//
//  1. limit 被夹到 [1, 20]：传 0 或 999 都不能突破边界，
//     否则调用方一次就能把整个会话的历史拉走；
//  2. 顺序是 created_at DESC, confirmation_id DESC：列表顶部必须是最新的那张，
//     用户回看历史时最先看到的应当是刚刚确认过的内容；
//  3. 只返回「该买家 + 该会话」的确认单：确认单里带着商品、金额与地址，
//     跨买家泄漏等于把别人的交易内容摊开给用户看。
//
// 干扰数据刻意排在最后（created_at 最新）：过滤条件若漏掉 buyer 或 session，
// 它们会出现在列表顶部，断言立刻失败，而不是被 limit 截掉后静默通过。
func TestLedgerExtraConfirmationsClampLimitAndScopeToOwner(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 100, 12900, catalog.CNY)
	ctx := context.Background()

	const own = 25
	created := make([]string, 0, own)
	for i := 0; i < own; i++ {
		if i > 0 {
			// 每张确认单间隔一秒：created_at 互不相同，顺序断言才有唯一解
			env.clock.Advance(time.Second)
		}
		confirmation := env.prepare(t, ledgerExtraCreateRequest(env,
			fmt.Sprintf("operation-own-%d", i), "buyer-1", "session-1"))
		created = append(created, confirmation.ConfirmationID)
	}

	env.clock.Advance(time.Second)
	foreignBuyer := env.prepare(t, ledgerExtraCreateRequest(env, "operation-foreign-buyer", "buyer-2", "session-1"))
	env.clock.Advance(time.Second)
	foreignSession := env.prepare(t, ledgerExtraCreateRequest(env, "operation-foreign-session", "buyer-1", "session-2"))

	newestFirst := make([]string, 0, len(created))
	for i := len(created) - 1; i >= 0; i-- {
		newestFirst = append(newestFirst, created[i])
	}

	clamped, err := env.store.Confirmations(ctx, "buyer-1", "session-1", 999)
	if err != nil {
		t.Fatalf("列出确认单失败：%v", err)
	}
	if got, want := ledgerExtraConfirmationIDs(clamped), newestFirst[:20]; !slices.Equal(got, want) {
		t.Errorf("limit=999 返回 %d 张，顺序 = %v，期望最新的 20 张 %v", len(got), got, want)
	}
	for _, id := range ledgerExtraConfirmationIDs(clamped) {
		if id == foreignBuyer.ConfirmationID || id == foreignSession.ConfirmationID {
			t.Errorf("列表泄漏了其它买家或会话的确认单：%s", id)
		}
	}

	t.Run("limit 为 0 被夹到 1", func(t *testing.T) {
		list, err := env.store.Confirmations(ctx, "buyer-1", "session-1", 0)
		if err != nil {
			t.Fatalf("列出确认单失败：%v", err)
		}
		if got := ledgerExtraConfirmationIDs(list); !slices.Equal(got, newestFirst[:1]) {
			t.Errorf("limit=0 返回 %v，期望只返回最新的一张 %v", got, newestFirst[:1])
		}
	})

	t.Run("limit 为 1 恰好一张", func(t *testing.T) {
		list, err := env.store.Confirmations(ctx, "buyer-1", "session-1", 1)
		if err != nil {
			t.Fatalf("列出确认单失败：%v", err)
		}
		if got := ledgerExtraConfirmationIDs(list); !slices.Equal(got, newestFirst[:1]) {
			t.Errorf("limit=1 返回 %v，期望 %v", got, newestFirst[:1])
		}
	})

	t.Run("limit 在区间内按原值生效", func(t *testing.T) {
		list, err := env.store.Confirmations(ctx, "buyer-1", "session-1", 2)
		if err != nil {
			t.Fatalf("列出确认单失败：%v", err)
		}
		if got := ledgerExtraConfirmationIDs(list); !slices.Equal(got, newestFirst[:2]) {
			t.Errorf("limit=2 返回 %v，期望 %v", got, newestFirst[:2])
		}
	})

	t.Run("干扰数据本身存在，只是被过滤掉", func(t *testing.T) {
		// 这两条断言让上面的「没出现」变得有意义：确认单确实写进了库，
		// 只有按 (buyer_id, session_id) 过滤才会把它们排除在外
		buyerScoped, err := env.store.Confirmations(ctx, "buyer-2", "session-1", 20)
		if err != nil {
			t.Fatalf("列出确认单失败：%v", err)
		}
		if got := ledgerExtraConfirmationIDs(buyerScoped); !slices.Equal(got, []string{foreignBuyer.ConfirmationID}) {
			t.Errorf("buyer-2 的列表 = %v，期望只有 %s", got, foreignBuyer.ConfirmationID)
		}

		sessionScoped, err := env.store.Confirmations(ctx, "buyer-1", "session-2", 20)
		if err != nil {
			t.Fatalf("列出确认单失败：%v", err)
		}
		if got := ledgerExtraConfirmationIDs(sessionScoped); !slices.Equal(got, []string{foreignSession.ConfirmationID}) {
			t.Errorf("session-2 的列表 = %v，期望只有 %s", got, foreignSession.ConfirmationID)
		}

		// 两个条件是与关系，不是或关系
		none, err := env.store.Confirmations(ctx, "buyer-2", "session-2", 20)
		if err != nil {
			t.Fatalf("列出确认单失败：%v", err)
		}
		if len(none) != 0 {
			t.Errorf("buyer-2 + session-2 应为空，实际 %v", ledgerExtraConfirmationIDs(none))
		}
	})
}

// TestLedgerExtraInventoryFiltersRequestedSKUs 覆盖带 sku_ids 的库存读取。
//
// 既有用例只传 nil（读全部），因此这条过滤分支此前完全没有断言：
// 它若退化成「返回全部」，调用方就会拿别的 SKU 的库存做决策。
func TestLedgerExtraInventoryFiltersRequestedSKUs(t *testing.T) {
	env := newEnv(t)
	env.seedProducts(t,
		seedSpec("sku-1", "p-1", 5, 12900, catalog.CNY),
		seedSpec("sku-2", "p-2", 7, 9900, catalog.CNY),
		seedSpec("sku-3", "p-3", 9, 100, catalog.CNY),
	)
	ctx := context.Background()

	stock, err := env.store.Inventory(ctx, []string{"sku-1", "sku-3"})
	if err != nil {
		t.Fatalf("读取指定 SKU 库存失败：%v", err)
	}
	if len(stock) != 2 {
		t.Errorf("返回 %d 个 SKU，期望恰好 2 个：%v", len(stock), stock)
	}
	if got := stock["sku-1"]; got != 5 {
		t.Errorf("sku-1 库存 = %d，期望 5", got)
	}
	if got := stock["sku-3"]; got != 9 {
		t.Errorf("sku-3 库存 = %d，期望 9", got)
	}
	if _, ok := stock["sku-2"]; ok {
		t.Errorf("未请求的 sku-2 不应出现在结果里：%v", stock)
	}

	t.Run("不存在的 SKU 只被忽略而不报错", func(t *testing.T) {
		missing, err := env.store.Inventory(ctx, []string{"sku-unknown"})
		if err != nil {
			t.Fatalf("读取不存在的 SKU 不应报错：%v", err)
		}
		if len(missing) != 0 {
			t.Errorf("结果 = %v，期望空", missing)
		}
	})

	t.Run("空列表与 nil 同义表示全部", func(t *testing.T) {
		all, err := env.store.Inventory(ctx, []string{})
		if err != nil {
			t.Fatalf("读取全部库存失败：%v", err)
		}
		if len(all) != 3 {
			t.Errorf("返回 %d 个 SKU，期望 3 个", len(all))
		}
	})
}

// TestLedgerExtraResolveRejectsCancelConfirmation 覆盖「拒绝一张取消确认单」。
//
// 这条路径最危险的地方在于「什么都没做」和「做了一半」很容易混淆：
// 拒绝取消必须既不回补库存，也不改订单状态，只留下一条可审计的拒绝记录。
// 若它误走了批准分支，用户拒绝之后库存会被凭空加回去，而订单还是 CONFIRMED——
// 之后任何一次真正的取消都会再加一遍。
func TestLedgerExtraResolveRejectsCancelConfirmation(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	ctx := context.Background()

	created := ledgerExtraCreateOrder(t, env, "operation-1", "buyer-1", "session-1", defaultItem(2))
	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Fatalf("下单后库存 = %d，期望 3", got)
	}

	cancellation := env.prepare(t, ledgerExtraCancelRequest(env,
		"operation-cancel", "buyer-1", "session-1", created.OrderID, "不想要了"))

	rejected := env.resolve(t, cancellation, trade.DecisionReject)
	if rejected.Status != trade.StatusRejected {
		t.Errorf("确认单状态 = %s，期望 rejected", rejected.Status)
	}
	if rejected.Result != nil {
		t.Errorf("拒绝取消不应带订单结果，实际 %+v", rejected.Result)
	}
	if rejected.ResolvedAt == nil {
		t.Error("拒绝也应记录决议时间，否则时间线上看不出它已被处理")
	}

	stored, err := env.store.Order(ctx, created.OrderID, "buyer-1")
	if err != nil {
		t.Fatalf("读取订单失败：%v", err)
	}
	if stored.Status != order.StatusConfirmed {
		t.Errorf("订单状态 = %s，期望仍是 CONFIRMED", stored.Status)
	}
	if stored.CancelReason != "" {
		t.Errorf("订单取消原因 = %q，期望为空", stored.CancelReason)
	}
	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Errorf("库存 = %d，期望未被回补（3）", got)
	}

	// trade_operations 必须恰好一行：多写一行会让「决议只执行一次」失去证据，
	// 少写一行则会让「用户明确拒绝过」这件事在审计里消失
	var count int
	if err := env.pool.QueryRow(ctx,
		`SELECT count(*) FROM trade_operations WHERE confirmation_id = $1`,
		cancellation.ConfirmationID).Scan(&count); err != nil {
		t.Fatalf("统计决议记录失败：%v", err)
	}
	if count != 1 {
		t.Fatalf("决议记录行数 = %d，期望恰好 1", count)
	}

	var (
		decision string
		approved bool
	)
	if err := env.pool.QueryRow(ctx,
		`SELECT decision, approved FROM trade_operations WHERE confirmation_id = $1`,
		cancellation.ConfirmationID).Scan(&decision, &approved); err != nil {
		t.Fatalf("读取决议记录失败：%v", err)
	}
	if decision != string(trade.DecisionReject) || approved {
		t.Errorf("决议记录 = (%s, approved=%v)，期望 (rejected, false)", decision, approved)
	}

	orders, lines, operations := env.rowCounts(t)
	if orders != 1 || lines != 1 || operations != 2 {
		t.Errorf("行数 = (orders %d, lines %d, operations %d)，期望 (1, 1, 2)：下单与拒绝各一条决议",
			orders, lines, operations)
	}

	// 拒绝是终态：这张确认单不能被改判为批准
	_, err = env.store.Resolve(ctx, cancellation.ConfirmationID, cancellation.BuyerID,
		cancellation.SessionID, cancellation.SnapshotHash, trade.DecisionApprove)
	assertCode(t, err, trade.CodeDecisionConflict)
}

// TestLedgerExtraResolveUnknownConfirmationAndInvalidDecision 覆盖决议的两个参数分支。
func TestLedgerExtraResolveUnknownConfirmationAndInvalidDecision(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	ctx := context.Background()

	confirmation := env.prepare(t, ledgerExtraCreateRequest(env, "operation-1", "buyer-1", "session-1"))

	_, err := env.store.Resolve(ctx, "0123456789abcdef0123456789abcdef", "buyer-1", "session-1",
		confirmation.SnapshotHash, trade.DecisionApprove)
	assertCode(t, err, trade.CodeNotFound)

	// 决议取值必须来自枚举：传一个自造字符串不能被当成「既非批准也非拒绝」
	_, err = env.store.Resolve(ctx, confirmation.ConfirmationID, "buyer-1", "session-1",
		confirmation.SnapshotHash, trade.Decision("maybe"))
	assertCode(t, err, trade.CodeInvalidArgument)

	// 参数被拒不能留下任何痕迹：确认单仍是 pending，随后仍可正常决议
	pending, err := env.store.Confirmation(ctx, confirmation.ConfirmationID, "buyer-1", "session-1")
	if err != nil {
		t.Fatalf("读取确认单失败：%v", err)
	}
	if pending.Status != trade.StatusPending {
		t.Errorf("确认单状态 = %s，期望仍为 pending", pending.Status)
	}
	if pending.ResolvedAt != nil {
		t.Error("未成功的决议不应写入决议时间")
	}

	env.resolve(t, confirmation, trade.DecisionApprove)
	orders, lines, operations := env.rowCounts(t)
	if orders != 1 || lines != 1 || operations != 1 {
		t.Errorf("行数 = (%d, %d, %d)，期望 (1, 1, 1)", orders, lines, operations)
	}
}
