package pg_test

import (
	"context"
	"errors"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	"github.com/NicoYazawa/crosspilot/internal/domain/trade"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
)

// 本文件覆盖取消路径的第一阶段校验、空目录初始化，以及取消回补库存的原子性。
//
// 取消是账本里唯一会「把货加回去」的操作，因此它每一处提前退出都必须
// 保证库存没被动过：多回补一次就是凭空造出一件不存在的货。

// insertLedgerExtraDraftOrder 直接写一张带明细的 DRAFT 订单。
//
// 故意绕过账本：账本唯一的订单构造入口是 order.NewConfirmed，
// 它只产出 CONFIRMED 订单，所以「非 CONFIRMED 订单」在账本里根本造不出来。
// 这类行来自历史数据或其它写入方，取消确认必须明确拒绝它们。
//
// 明细一并写入也是刻意的：没有明细的订单会先撞上「明细不合法」这条判定，
// 那样测到的就不是「状态不对」这条规则了。
func insertLedgerExtraDraftOrder(t *testing.T, env *testEnv, orderID string) {
	t.Helper()
	ctx := context.Background()

	if _, err := env.pool.Exec(ctx, `
INSERT INTO orders (order_id, buyer_id, status, currency, total_amount_minor,
    shipping_address_json, created_at)
VALUES ($1, 'buyer-1', 'DRAFT', 'CNY', 100, '{}', $2)`, orderID, fixedNow); err != nil {
		t.Fatalf("写入草稿订单失败：%v", err)
	}
	if _, err := env.pool.Exec(ctx, `
INSERT INTO order_lines (order_id, sku_id, product_id, title, unit_price_minor, currency, quantity)
VALUES ($1, 'sku-1', 'p-1', '草稿商品', 100, 'CNY', 1)`, orderID); err != nil {
		t.Fatalf("写入草稿订单行失败：%v", err)
	}
}

// TestLedgerExtraPrepareCancelValidation 覆盖取消确认建立前的四条校验。
//
// 它们都在写库之前返回，因此还要顺带断言「被拒绝的请求没有留下确认单」：
// 校验失败却写入了一行 pending，用户就会在列表里看到一张永远无法决议的确认单。
func TestLedgerExtraPrepareCancelValidation(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	ctx := context.Background()

	created := ledgerExtraCreateOrder(t, env, "operation-1", "buyer-1", "session-1", defaultItem(2))

	confirmationsOf := func(t *testing.T) int {
		t.Helper()
		var count int
		if err := env.pool.QueryRow(ctx, `SELECT count(*) FROM trade_confirmations`).Scan(&count); err != nil {
			t.Fatalf("统计确认单失败：%v", err)
		}
		return count
	}
	before := confirmationsOf(t)

	t.Run("订单不存在返回未找到", func(t *testing.T) {
		_, err := env.store.Prepare(ctx, ledgerExtraCancelRequest(env,
			"cancel-missing", "buyer-1", "session-1", "0123456789abcdef0123456789abcdef", "不想要了"))
		assertCode(t, err, trade.CodeNotFound)
	})

	t.Run("订单属于别的买家返回归属不符", func(t *testing.T) {
		_, err := env.store.Prepare(ctx, ledgerExtraCancelRequest(env,
			"cancel-other-buyer", "buyer-2", "session-2", created.OrderID, "不想要了"))
		assertCode(t, err, trade.CodeOwnerMismatch)
	})

	t.Run("订单不是已确认状态返回订单已变化", func(t *testing.T) {
		const draftOrderID = "fedcba9876543210fedcba9876543210"
		insertLedgerExtraDraftOrder(t, env, draftOrderID)

		_, err := env.store.Prepare(ctx, ledgerExtraCancelRequest(env,
			"cancel-draft", "buyer-1", "session-1", draftOrderID, "不想要了"))
		assertCode(t, err, trade.CodeOrderChanged)
	})

	// 上面三条都必须在写库之前失败：被拒绝的「准备」若留下一行 pending，
	// 用户就会在列表里看到一张永远无法决议的确认单
	if after := confirmationsOf(t); after != before {
		t.Errorf("确认单总数 = %d，期望仍为 %d（被拒绝的准备不应落库）", after, before)
	}

	// 取消重放必须成立：取消载荷要靠订单快照重建，因此存储层在重放分支上
	// 也要读订单，否则领域层算不出请求摘要，端口契约里
	// 「重复调用返回首次建立的那张确认单」在取消动作上就是空的。
	t.Run("同一操作编号原样重放复用既有确认单", func(t *testing.T) {
		first := env.prepare(t, ledgerExtraCancelRequest(env,
			"cancel-replay", "buyer-1", "session-1", created.OrderID, "原因一"))

		replayed, err := env.store.Prepare(ctx, ledgerExtraCancelRequest(env,
			"cancel-replay", "buyer-1", "session-1", created.OrderID, "原因一"))
		if err != nil {
			t.Fatalf("原样重放不应报错：%v", err)
		}
		if replayed.ConfirmationID != first.ConfirmationID {
			t.Errorf("重放确认单 = %s，期望复用 %s", replayed.ConfirmationID, first.ConfirmationID)
		}
		if replayed.Status != trade.StatusPending {
			t.Errorf("重放状态 = %s，期望 pending", replayed.Status)
		}

		// 重放必须是「原样返回既有那一行」，而不是「按当前订单重新生成一份」：
		// 摘要与有效期参与快照校验，被改写就等于把用户批准的那份内容换掉了。
		if !replayed.ExpiresAt.Equal(first.ExpiresAt) {
			t.Errorf("重放有效期 = %s，期望保持 %s", replayed.ExpiresAt, first.ExpiresAt)
		}
		if !replayed.CreatedAt.Equal(first.CreatedAt) {
			t.Errorf("重放创建时间 = %s，期望保持 %s", replayed.CreatedAt, first.CreatedAt)
		}
		if replayed.SnapshotHash != first.SnapshotHash {
			t.Errorf("重放快照摘要 = %s，期望保持 %s", replayed.SnapshotHash, first.SnapshotHash)
		}
		if replayed.RequestHash != first.RequestHash {
			t.Errorf("重放请求摘要 = %s，期望保持 %s", replayed.RequestHash, first.RequestHash)
		}
		if replayed.ResolvedAt != nil || replayed.Result != nil {
			t.Errorf("尚未决议的重放不应带决议信息：resolved_at=%v result=%+v", replayed.ResolvedAt, replayed.Result)
		}
		if replayed.Payload.Cancel == nil || replayed.Payload.Create != nil {
			t.Fatalf("重放载荷应仍是取消载荷：%+v", replayed.Payload)
		}
		// 取消载荷逐字段一致：订单号、原因、订单状态、明细、地址、金额、币种
		if !trade.CancelPayloadMatches(*first.Payload.Cancel, *replayed.Payload.Cancel) {
			t.Errorf("重放载荷 = %+v，期望与首次完全一致 %+v", *replayed.Payload.Cancel, *first.Payload.Cancel)
		}
		if replayed.Payload.Cancel.Reason != "原因一" || replayed.Payload.Cancel.TotalAmountMinor != 25800 {
			t.Errorf("重放载荷（原因, 总额）= (%q, %d)，期望 (原因一, 25800)",
				replayed.Payload.Cancel.Reason, replayed.Payload.Cancel.TotalAmountMinor)
		}

		// 同一操作编号在库里仍然只有一行，且没有产生任何决议记录
		var rows int
		if err := env.pool.QueryRow(ctx,
			`SELECT count(*) FROM trade_confirmations WHERE operation_id = 'cancel-replay'`).Scan(&rows); err != nil {
			t.Fatalf("统计确认单失败：%v", err)
		}
		if rows != 1 {
			t.Errorf("operation_id = cancel-replay 下有 %d 张确认单，期望恰好 1 张", rows)
		}
		var operations int
		if err := env.pool.QueryRow(ctx,
			`SELECT count(*) FROM trade_operations WHERE confirmation_id = $1`,
			first.ConfirmationID).Scan(&operations); err != nil {
			t.Fatalf("统计决议记录失败：%v", err)
		}
		if operations != 0 {
			t.Errorf("决议记录行数 = %d，期望 0（重放不是决议）", operations)
		}
	})

	t.Run("同一操作编号换原因返回操作冲突", func(t *testing.T) {
		first := env.prepare(t, ledgerExtraCancelRequest(env,
			"cancel-conflict", "buyer-1", "session-1", created.OrderID, "原因一"))

		_, err := env.store.Prepare(ctx, ledgerExtraCancelRequest(env,
			"cancel-conflict", "buyer-1", "session-1", created.OrderID, "原因二"))
		assertCode(t, err, trade.CodeOperationConflict)

		// 冲突是「拒绝写入」而不是「改写」：既有确认单必须原封不动，
		// 否则用户批准的「原因一」会被后一次请求悄悄替换成「原因二」
		stored, err := env.store.Confirmation(ctx, first.ConfirmationID, "buyer-1", "session-1")
		if err != nil {
			t.Fatalf("读取既有确认单失败：%v", err)
		}
		if stored.Payload.Cancel == nil || stored.Payload.Cancel.Reason != "原因一" {
			t.Errorf("既有确认单原因 = %+v，期望保持「原因一」", stored.Payload.Cancel)
		}
		if stored.SnapshotHash != first.SnapshotHash || stored.RequestHash != first.RequestHash {
			t.Errorf("既有确认单摘要被改写：snapshot %s→%s，request %s→%s",
				first.SnapshotHash, stored.SnapshotHash, first.RequestHash, stored.RequestHash)
		}
		if !stored.ExpiresAt.Equal(first.ExpiresAt) {
			t.Errorf("既有确认单有效期被改写：%s → %s", first.ExpiresAt, stored.ExpiresAt)
		}
		if stored.Status != trade.StatusPending {
			t.Errorf("既有确认单状态 = %s，期望仍为 pending", stored.Status)
		}
	})

	t.Run("已取消的订单不能再次发起取消", func(t *testing.T) {
		victim := ledgerExtraCreateOrder(t, env, "operation-victim", "buyer-1", "session-1")
		env.resolve(t, env.prepare(t, ledgerExtraCancelRequest(env,
			"victim-cancel-1", "buyer-1", "session-1", victim.OrderID, "不想要了")), trade.DecisionApprove)

		cancelled, err := env.store.Order(ctx, victim.OrderID, "buyer-1")
		if err != nil {
			t.Fatalf("读取订单失败：%v", err)
		}
		if cancelled.Status != order.StatusCancelled {
			t.Fatalf("订单状态 = %s，期望 CANCELLED", cancelled.Status)
		}

		// 换一个全新的操作编号再发起一次取消：订单已不是 CONFIRMED，
		// 取消载荷根本建不出来，必须在「准备」阶段就拒绝，
		// 而不是等用户点了确认才告诉他这单已经取消了
		_, err = env.store.Prepare(ctx, ledgerExtraCancelRequest(env,
			"victim-cancel-2", "buyer-1", "session-1", victim.OrderID, "再取消一次"))
		assertCode(t, err, trade.CodeOrderChanged)
	})
}

// TestLedgerExtraInitializeInventoryEmptyIsNoop 覆盖空目录初始化。
//
// 「空目录」在首启或目录为空时是真实输入，它必须是无操作而不是「清空库存」：
// 一旦被理解成后者，一次空目录发布就会把所有可售数量抹掉。
func TestLedgerExtraInitializeInventoryEmptyIsNoop(t *testing.T) {
	env := newEnv(t)
	env.seedProducts(t,
		seedSpec("sku-1", "p-1", 5, 12900, catalog.CNY),
		seedSpec("sku-2", "p-2", 7, 9900, catalog.CNY),
	)
	ctx := context.Background()

	before := env.inventory(t)

	if err := env.store.InitializeInventory(ctx, nil); err != nil {
		t.Fatalf("nil 目录应当是无操作，实际：%v", err)
	}
	if err := env.store.InitializeInventory(ctx, []trade.SeedSKU{}); err != nil {
		t.Fatalf("空切片目录应当是无操作，实际：%v", err)
	}

	after := env.inventory(t)
	if len(after) != len(before) {
		t.Fatalf("库存行数 = %d，期望保持 %d（空目录不得删除任何行）", len(after), len(before))
	}
	for skuID, stock := range before {
		if after[skuID] != stock {
			t.Errorf("%s 库存 = %d，期望保持 %d", skuID, after[skuID], stock)
		}
	}
}

// TestLedgerExtraCancelFaultAfterStockRestoreRollsBackBoth 用故障注入证明取消的原子性。
//
// 注入点 FaultAfterInventoryRestore 位于「库存已回补、订单状态还没改」之间，
// 这是取消事务里最危险的中间态：只回滚一半就会留下「库存已经回来了、
// 但订单还是 CONFIRMED」的账目——此后任何一次真正的取消都会再把同一批货
// 加回一遍，凭空多出库存。
func TestLedgerExtraCancelFaultAfterStockRestoreRollsBackBoth(t *testing.T) {
	injected := errors.New("模拟回补库存后写订单状态失败")
	env := newFaultEnv(t, "resolve_confirmation", pg.FaultAfterInventoryRestore, injected)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	ctx := context.Background()

	created := env.resolve(t, env.prepare(t, createRequest("operation-1", defaultItem(2))), trade.DecisionApprove)
	if created.Result == nil {
		t.Fatalf("下单应返回订单结果：%+v", created)
	}
	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Fatalf("下单后库存 = %d，期望 3", got)
	}

	cancellation := env.prepare(t, ledgerExtraCancelRequest(env.testEnv,
		"operation-cancel", "buyer-1", "session-1", created.Result.OrderID, "不想要了"))

	_, err := env.store.Resolve(ctx, cancellation.ConfirmationID, cancellation.BuyerID,
		cancellation.SessionID, cancellation.SnapshotHash, trade.DecisionApprove)
	if !errors.Is(err, injected) {
		t.Fatalf("错误应透传注入的失败原因，实际：%v", err)
	}
	if got := env.hitCount(); got != 1 {
		t.Errorf("注入点命中 %d 次，期望恰好 1 次", got)
	}

	// 回补与状态更新必须一起回滚：库存回到取消前的值，订单仍是 CONFIRMED
	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Errorf("库存 = %d，期望回滚到取消前的 3", got)
	}
	stored, err := env.store.Order(ctx, created.Result.OrderID, "buyer-1")
	if err != nil {
		t.Fatalf("读取订单失败：%v", err)
	}
	if stored.Status != order.StatusConfirmed {
		t.Errorf("订单状态 = %s，期望仍为 CONFIRMED", stored.Status)
	}
	if stored.CancelReason != "" {
		t.Errorf("订单取消原因 = %q，期望为空", stored.CancelReason)
	}

	// 确认单也必须保持 pending：留在 pending 才说明这次取消从未生效，
	// 用户重试时不会被判成「已经决议过」
	pending, err := env.store.Confirmation(ctx, cancellation.ConfirmationID, "buyer-1", "session-1")
	if err != nil {
		t.Fatalf("读取确认单失败：%v", err)
	}
	if pending.Status != trade.StatusPending {
		t.Errorf("确认单状态 = %s，期望仍为 pending", pending.Status)
	}
	if pending.ResolvedAt != nil {
		t.Error("失败的取消不应写入决议时间")
	}

	orders, lines, operations := env.rowCounts(t)
	if orders != 1 || lines != 1 || operations != 1 {
		t.Errorf("行数 = (%d, %d, %d)，期望只有下单那一笔的 (1, 1, 1)", orders, lines, operations)
	}

	// 故障排除后必须能干净地重做这次取消：回滚不能留下「已经回补过」的痕迹，
	// 否则库存会多加一倍
	env.clearFaults()
	resolved, err := env.store.Resolve(ctx, cancellation.ConfirmationID, cancellation.BuyerID,
		cancellation.SessionID, cancellation.SnapshotHash, trade.DecisionApprove)
	if err != nil {
		t.Fatalf("故障排除后重做取消失败：%v", err)
	}
	if resolved.Result == nil || resolved.Result.Status != order.StatusCancelled {
		t.Errorf("重做后的订单结果 = %+v，期望 CANCELLED", resolved.Result)
	}
	if got := env.inventory(t)["sku-1"]; got != 5 {
		t.Errorf("重做后库存 = %d，期望只回补一次（5）", got)
	}
	orders, lines, operations = env.rowCounts(t)
	if orders != 1 || lines != 1 || operations != 2 {
		t.Errorf("行数 = (%d, %d, %d)，期望 (1, 1, 2)", orders, lines, operations)
	}
}
