package pg_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	"github.com/NicoYazawa/crosspilot/internal/domain/session"
	"github.com/NicoYazawa/crosspilot/internal/domain/trade"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg/pgtest"
)

// pgtestDatabase 建一个独立测试库并执行迁移。
func pgtestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return pgtest.NewDatabase(t, func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pg.Migrate(ctx, pool, pg.Up)
		return err
	})
}

// faultEnv 是一套可以注入故障的账本环境。
//
// 故障只在指定操作上触发：注入点位于事务内部，若不限定操作，
// 连「准备测试数据」这步都会被注入失败，测试就测不到想测的东西。
type faultEnv struct {
	*testEnv
	mu      sync.Mutex
	op      string
	point   pg.FaultPoint
	err     error
	hits    int
	enabled bool
}

// newFaultEnv 装配一套带故障注入的测试环境。故障只在 operation 参数上生效。
func newFaultEnv(t *testing.T, operation string, point pg.FaultPoint, injected error) *faultEnv {
	t.Helper()

	pool := pgtestDatabase(t)
	clock := &testClock{now: fixedNow}
	env := &faultEnv{
		testEnv: &testEnv{pool: pool, clock: clock},
		op:      operation,
		point:   point,
		err:     injected,
		enabled: true,
	}

	store, err := pg.New(pg.Config{
		Pool:  pool,
		Clock: clock,
		Faults: func(got pg.FaultPoint) error {
			env.mu.Lock()
			defer env.mu.Unlock()
			if !env.enabled || got != env.point {
				return nil
			}
			env.hits++
			return env.err
		},
		FaultOperations: func(operation string) bool {
			return operation == env.op
		},
	})
	if err != nil {
		t.Fatalf("装配带故障注入的存储失败：%v", err)
	}
	env.store = store
	return env
}

// hitCount 返回故障被触发的次数。
func (e *faultEnv) hitCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hits
}

// clearFaults 取消故障注入。
func (e *faultEnv) clearFaults() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enabled = false
}

// --- B3：事务原子性（故障注入） ---

func TestResolveRollsBackEverythingWhenWritingFails(t *testing.T) {
	// 三个注入点分别落在「扣完库存还没写订单」「写完订单还没记决议」
	// 「全部写完还没提交」——覆盖事务里最容易被漏掉的三个中间态。
	points := []pg.FaultPoint{
		pg.FaultAfterInventoryDebit,
		pg.FaultAfterOrderInsert,
		pg.FaultBeforeCommit,
	}

	for _, point := range points {
		t.Run(string(point), func(t *testing.T) {
			injected := errors.New("模拟持久化失败")
			env := newFaultEnv(t, "resolve_confirmation", point, injected)
			env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
			confirmation := env.prepare(t, createRequest("operation-1", defaultItem(2)))

			_, err := env.store.Resolve(context.Background(),
				confirmation.ConfirmationID, confirmation.BuyerID, confirmation.SessionID,
				confirmation.SnapshotHash, trade.DecisionApprove)
			if !errors.Is(err, injected) {
				t.Fatalf("错误应透传原始失败原因，实际：%v", err)
			}
			if got := env.hitCount(); got != 1 {
				t.Errorf("注入点命中 %d 次，期望恰好 1 次（业务失败不应被当成冲突重试）", got)
			}

			if got := env.inventory(t)["sku-1"]; got != 5 {
				t.Errorf("库存 = %d，期望回滚到 5", got)
			}
			orders, lines, operations := env.rowCounts(t)
			if orders != 0 || lines != 0 || operations != 0 {
				t.Errorf("行数 = (%d, %d, %d)，期望全部回滚为 0", orders, lines, operations)
			}

			stored, readErr := env.store.Confirmation(context.Background(),
				confirmation.ConfirmationID, confirmation.BuyerID, confirmation.SessionID)
			if readErr != nil {
				t.Fatalf("读取确认单失败：%v", readErr)
			}
			if stored.Status != trade.StatusPending {
				t.Errorf("确认单状态 = %s，期望仍为 pending", stored.Status)
			}

			// 故障排除后必须能重新完成这笔交易：回滚应当是干净的，
			// 不能留下「扣过一次库存」这类看不见的残留。
			env.clearFaults()
			resolved, resolveErr := env.store.Resolve(context.Background(),
				confirmation.ConfirmationID, confirmation.BuyerID, confirmation.SessionID,
				confirmation.SnapshotHash, trade.DecisionApprove)
			if resolveErr != nil {
				t.Fatalf("故障排除后重新决议失败：%v", resolveErr)
			}
			if resolved.Status != trade.StatusApproved {
				t.Errorf("状态 = %s，期望 approved", resolved.Status)
			}
			if got := env.inventory(t)["sku-1"]; got != 3 {
				t.Errorf("故障排除后库存 = %d，期望 3（只扣一次）", got)
			}
		})
	}
}

func TestLaterSKUShortageRollsBackEarlierDebit(t *testing.T) {
	env := newEnv(t)
	// 两个 SKU 都给足：prepare 会逐项核对库存，库存不足时连确认单都建不出来，
	// 也就测不到「决议中途失败」这件事。
	env.seedProducts(t,
		seedSpec("sku-1", "p-1", 5, 12900, catalog.CNY),
		seedSpec("sku-2", "p-2", 5, 9900, catalog.CNY),
	)
	multi := env.prepare(t, createRequest("operation-multi",
		item("sku-1", "p-1", 2, 12900, catalog.CNY),
		item("sku-2", "p-2", 4, 9900, catalog.CNY)))

	// 确认单建好之后把 sku-2 掏空：决议时 sku-1 会先被扣掉，
	// 随后 sku-2 失败，整个事务必须回滚掉已经扣掉的 sku-1。
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE trade_sku_inventory SET stock = 0 WHERE sku_id = 'sku-2'`); err != nil {
		t.Fatalf("清空 sku-2 库存失败：%v", err)
	}

	_, err := env.store.Resolve(context.Background(),
		multi.ConfirmationID, multi.BuyerID, multi.SessionID,
		multi.SnapshotHash, trade.DecisionApprove)
	assertCode(t, err, trade.CodeInsufficientStock)

	stock := env.inventory(t)
	if stock["sku-1"] != 5 {
		t.Errorf("sku-1 库存 = %d，期望回滚到 5", stock["sku-1"])
	}
	if stock["sku-2"] != 0 {
		t.Errorf("sku-2 库存 = %d，期望 0", stock["sku-2"])
	}
	orders, lines, operations := env.rowCounts(t)
	if orders != 0 || lines != 0 || operations != 0 {
		t.Errorf("行数 = (%d, %d, %d)，期望整笔交易回滚为 0", orders, lines, operations)
	}
}

// --- B5：价格漂移防护 ---

func TestPriceDriftAfterPrepareRejectsResolve(t *testing.T) {
	cases := []struct {
		name     string
		price    int64
		currency catalog.Currency
	}{
		{"价格变化", 9900, catalog.CNY},
		{"币种变化", 12900, catalog.USD},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
			confirmation := env.prepare(t, createRequest("operation-1", defaultItem(2)))

			// 确认单生成之后权威报价变了：展示给买家的价格不再有效
			env.seedProduct(t, "sku-1", "p-1", 5, tc.price, tc.currency)

			_, err := env.store.Resolve(context.Background(),
				confirmation.ConfirmationID, confirmation.BuyerID, confirmation.SessionID,
				confirmation.SnapshotHash, trade.DecisionApprove)
			assertCode(t, err, trade.CodePriceChanged)

			if got := env.inventory(t)["sku-1"]; got != 5 {
				t.Errorf("库存 = %d，期望未被扣减", got)
			}
			orders, lines, operations := env.rowCounts(t)
			if orders != 0 || lines != 0 || operations != 0 {
				t.Errorf("行数 = (%d, %d, %d)，期望全部为 0", orders, lines, operations)
			}
		})
	}
}

// --- 库存初始化语义 ---

func TestInitializeInventoryPreservesSoldStock(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)

	confirmation := env.prepare(t, createRequest("operation-1", defaultItem(2)))
	env.resolve(t, confirmation, trade.DecisionApprove)
	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Fatalf("卖出后库存 = %d，期望 3", got)
	}

	t.Run("重跑初始化不重置库存", func(t *testing.T) {
		env.seedProduct(t, "sku-1", "p-1", 999, 12900, catalog.CNY)
		if got := env.inventory(t)["sku-1"]; got != 3 {
			t.Errorf("库存 = %d，期望 3（重启不得把卖掉的货变回来）", got)
		}
	})

	t.Run("重跑初始化会刷新报价", func(t *testing.T) {
		env.seedProduct(t, "sku-1", "p-1", 999, 13900, catalog.CNY)
		next := env.prepare(t, createRequest("operation-2", item("sku-1", "p-1", 1, 13900, catalog.CNY)))
		if next.Payload.Create.Items[0].UnitPriceMinor != 13900 {
			t.Errorf("报价未刷新：%d", next.Payload.Create.Items[0].UnitPriceMinor)
		}
	})

	t.Run("已有 SKU 不可迁移到另一个商品", func(t *testing.T) {
		err := env.store.InitializeInventory(context.Background(),
			[]trade.SeedSKU{seedSpec("sku-1", "p-other", 1, 12900, catalog.CNY)})
		assertCode(t, err, trade.CodeInvalidArgument)
	})

	t.Run("目录内重复 SKU 被拒绝", func(t *testing.T) {
		err := env.store.InitializeInventory(context.Background(), []trade.SeedSKU{
			seedSpec("sku-dup", "p-1", 1, 100, catalog.CNY),
			seedSpec("sku-dup", "p-1", 1, 100, catalog.CNY),
		})
		assertCode(t, err, trade.CodeInvalidArgument)
	})

	t.Run("并发初始化是串行的", func(t *testing.T) {
		// 首启时多个实例可能同时灌种子数据：没有串行化就会各自算出
		// 「这个 SKU 还没建」然后抢着插入，或更糟——互相覆盖库存。
		fresh := newEnv(t)
		specs := []trade.SeedSKU{seedSpec("sku-race", "p-1", 7, 100, catalog.CNY)}

		var wg sync.WaitGroup
		errs := make([]error, 4)
		start := make(chan struct{})
		for i := 0; i < len(errs); i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = newStoreFor(t, fresh).InitializeInventory(context.Background(), specs)
			}()
		}
		close(start)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("第 %d 个并发初始化失败：%v", i, err)
			}
		}
		if got := fresh.inventory(t)["sku-race"]; got != 7 {
			t.Errorf("库存 = %d，期望恰好 7（种子只应生效一次）", got)
		}
	})

	t.Run("首次初始化扣掉历史订单占用", func(t *testing.T) {
		// 模拟「订单已经在库里，但还没有库存行」——直接写一张已确认历史订单
		fresh := newEnv(t)
		insertLegacyConfirmedOrder(t, fresh, "legacy-order-a", "legacy-sku", "legacy-p", 2)

		err := fresh.store.InitializeInventory(context.Background(),
			[]trade.SeedSKU{seedSpec("legacy-sku", "legacy-p", 5, 100, catalog.CNY)})
		if err != nil {
			t.Fatalf("初始化失败：%v", err)
		}
		if got := fresh.inventory(t)["legacy-sku"]; got != 3 {
			t.Errorf("库存 = %d，期望 5-2=3", got)
		}
	})

	t.Run("历史占用超出种子时报迁移错误且不留行", func(t *testing.T) {
		fresh := newEnv(t)
		insertLegacyConfirmedOrder(t, fresh, "legacy-order-b", "legacy-sku", "legacy-p", 6)

		err := fresh.store.InitializeInventory(context.Background(),
			[]trade.SeedSKU{seedSpec("legacy-sku", "legacy-p", 5, 100, catalog.CNY)})
		assertCode(t, err, trade.CodeInventoryMigrationRequired)

		if _, ok := fresh.inventory(t)["legacy-sku"]; ok {
			t.Error("失败的初始化不应留下半迁移的库存行")
		}
	})
}

// insertLegacyConfirmedOrder 直接写一张已确认的历史订单，绕过两阶段提交。
func insertLegacyConfirmedOrder(t *testing.T, env *testEnv, orderID, skuID, productID string, quantity int64) {
	t.Helper()

	ctx := context.Background()
	if _, err := env.pool.Exec(ctx, `
INSERT INTO orders (order_id, buyer_id, status, currency, total_amount_minor,
    shipping_address_json, created_at, confirmed_at)
VALUES ($1, 'buyer-1', 'CONFIRMED', 'CNY', $2, '{}', $3, $3)`,
		orderID, quantity*100, fixedNow); err != nil {
		t.Fatalf("写入历史订单失败：%v", err)
	}
	if _, err := env.pool.Exec(ctx, `
INSERT INTO order_lines (order_id, sku_id, product_id, title, unit_price_minor, currency, quantity)
VALUES ($1, $2, $3, '旧商品', 100, 'CNY', $4)`,
		orderID, skuID, productID, quantity); err != nil {
		t.Fatalf("写入历史订单行失败：%v", err)
	}
}

// --- 取消的两阶段提交 ---

func TestCancelIsAtomicAndRestoresStockOnce(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)

	created := env.resolve(t, env.prepare(t, createRequest("operation-1", defaultItem(2))), trade.DecisionApprove)
	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Fatalf("下单后库存 = %d，期望 3", got)
	}

	cancellation := env.prepare(t, trade.PrepareRequest{
		OperationID: "operation-cancel",
		BuyerID:     "buyer-1",
		SessionID:   "session-1",
		Action:      trade.ActionCancel,
		OrderID:     created.Result.OrderID,
		Reason:      "不想要了",
		ExpiresAt:   fixedNow.Add(5 * time.Minute),
	})

	if cancellation.Payload.Cancel == nil {
		t.Fatal("应为取消载荷")
	}
	if got := cancellation.Payload.Cancel.TotalAmountMinor; got != 25800 {
		t.Errorf("取消载荷总额 = %d，期望 25800", got)
	}
	// 取消载荷必须带上订单上的收货地址：决议时要靠它发现「订单被改过」。
	// 真实调用方不会在取消请求里再传一次地址，地址只能来自订单本身。
	if got := cancellation.Payload.Cancel.ShippingAddress; got != address() {
		t.Errorf("取消载荷地址 = %+v，期望取自订单（而非请求）", got)
	}
	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Errorf("准备取消不应回补库存，实际 %d", got)
	}

	t.Run("并发决议只回补一次", func(t *testing.T) {
		other := newStoreFor(t, env)
		results, errs := resolveConcurrently(t,
			resolveCall{store: env.store, confirmation: cancellation},
			resolveCall{store: other, confirmation: cancellation},
		)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("第 %d 个取消决议失败：%v", i, err)
			}
		}
		if results[0].Result == nil || results[1].Result == nil {
			t.Fatalf("两个决议都应返回结果：%+v", results)
		}
		if results[0].Result.Status != order.StatusCancelled {
			t.Errorf("订单状态 = %s，期望 CANCELLED", results[0].Result.Status)
		}
		if got := env.inventory(t)["sku-1"]; got != 5 {
			t.Errorf("库存 = %d，期望回补到 5", got)
		}
		orders, lines, operations := env.rowCounts(t)
		if orders != 1 || lines != 1 || operations != 2 {
			t.Errorf("行数 = (%d, %d, %d)，期望 (1, 1, 2)", orders, lines, operations)
		}
	})

	t.Run("订单状态已是 CANCELLED", func(t *testing.T) {
		stored, err := env.store.Order(context.Background(), created.Result.OrderID, "buyer-1")
		if err != nil {
			t.Fatalf("读取订单失败：%v", err)
		}
		if stored.Status != order.StatusCancelled {
			t.Errorf("订单状态 = %s，期望 CANCELLED", stored.Status)
		}
		if stored.CancelReason != "不想要了" {
			t.Errorf("取消原因 = %q", stored.CancelReason)
		}
	})
}

func TestTwoCancelConfirmationsCannotRestoreStockTwice(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	created := env.resolve(t, env.prepare(t, createRequest("operation-1", defaultItem(2))), trade.DecisionApprove)

	first := env.prepare(t, trade.PrepareRequest{
		OperationID: "cancel-a", BuyerID: "buyer-1", SessionID: "session-1",
		Action: trade.ActionCancel, OrderID: created.Result.OrderID, Reason: "原因一",
		ExpiresAt: fixedNow.Add(5 * time.Minute),
	})
	second := env.prepare(t, trade.PrepareRequest{
		OperationID: "cancel-b", BuyerID: "buyer-1", SessionID: "session-1",
		Action: trade.ActionCancel, OrderID: created.Result.OrderID, Reason: "原因二",
		ExpiresAt: fixedNow.Add(5 * time.Minute),
	})

	env.resolve(t, first, trade.DecisionApprove)

	_, err := env.store.Resolve(context.Background(),
		second.ConfirmationID, second.BuyerID, second.SessionID,
		second.SnapshotHash, trade.DecisionApprove)
	assertCode(t, err, trade.CodeOrderChanged)

	if got := env.inventory(t)["sku-1"]; got != 5 {
		t.Errorf("库存 = %d，期望只回补一次（5）", got)
	}
	stored, err := env.store.Confirmation(context.Background(),
		second.ConfirmationID, second.BuyerID, second.SessionID)
	if err != nil {
		t.Fatalf("读取失败：%v", err)
	}
	if stored.Status != trade.StatusPending {
		t.Errorf("失败的确认单状态 = %s，期望仍为 pending", stored.Status)
	}
}

func TestCancelDetectsOrderMutation(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	created := env.resolve(t, env.prepare(t, createRequest("operation-1", defaultItem(2))), trade.DecisionApprove)

	cancellation := env.prepare(t, trade.PrepareRequest{
		OperationID: "cancel-a", BuyerID: "buyer-1", SessionID: "session-1",
		Action: trade.ActionCancel, OrderID: created.Result.OrderID, Reason: "不想要了",
		ExpiresAt: fixedNow.Add(5 * time.Minute),
	})

	// 等到决议之前改动订单内容：用户批准的那份快照已经不再成立
	if _, err := env.pool.Exec(context.Background(),
		`UPDATE orders SET shipping_address_json = '{"city":"北京"}' WHERE order_id = $1`,
		created.Result.OrderID); err != nil {
		t.Fatalf("改动订单失败：%v", err)
	}

	_, err := env.store.Resolve(context.Background(),
		cancellation.ConfirmationID, cancellation.BuyerID, cancellation.SessionID,
		cancellation.SnapshotHash, trade.DecisionApprove)
	assertCode(t, err, trade.CodeOrderChanged)

	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Errorf("库存 = %d，期望未回补（3）", got)
	}
}

// --- B7：冲突重试有界 ---

func TestConflictRetryIsBounded(t *testing.T) {
	t.Run("冲突次数在上限内时成功", func(t *testing.T) {
		// 前两次事务以序列化失败告终，第三次成功。
		// 这正是真实并发下的形态：一次冲突不代表这次调用该失败。
		env := newRetryEnv(t, 2)
		env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)

		confirmation, err := env.store.Prepare(context.Background(), createRequest("operation-1", defaultItem(2)))
		if err != nil {
			t.Fatalf("应当重试成功，实际：%v", err)
		}
		if confirmation.ConfirmationID == "" {
			t.Error("应返回确认单")
		}
		if got := env.attempts(); got < 3 {
			t.Errorf("尝试次数 = %d，期望至少 3", got)
		}
	})

	t.Run("冲突超过上限时放弃并给出明确错误", func(t *testing.T) {
		env := newRetryEnv(t, 100)
		env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)

		start := time.Now()
		_, err := env.store.Prepare(context.Background(), createRequest("operation-1", defaultItem(2)))
		if err == nil {
			t.Fatal("超过重试上限后应当失败")
		}
		if !strings.Contains(err.Error(), "重试") {
			t.Errorf("错误信息应说明重试耗尽，实际：%v", err)
		}
		if got := env.attempts(); got != 4 {
			t.Errorf("尝试次数 = %d，期望上限 4（首次 + 3 次重试）", got)
		}
		// 有界意味着耗时可预期：退避是 5ms/10ms/15ms 量级，
		// 若退避写成指数或固定大值，这里就会明显超时
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("重试耗时 %s，期望远小于 5s", elapsed)
		}
	})

	t.Run("业务失败不触发重试", func(t *testing.T) {
		// 库存不足是确定性结果，重试多少次都一样。
		// 把它当成可重试错误会让一次必然失败的操作白白重放三遍。
		env := newRetryEnv(t, 0)
		env.seedProduct(t, "sku-1", "p-1", 1, 12900, catalog.CNY)

		_, err := env.store.Prepare(context.Background(), createRequest("operation-1", defaultItem(2)))
		assertCode(t, err, trade.CodeInsufficientStock)
		if got := env.attempts(); got != 1 {
			t.Errorf("尝试次数 = %d，期望 1（确定性失败不重试）", got)
		}
	})
}

// retryEnv 是一个按次数注入可重试冲突的环境。
type retryEnv struct {
	*testEnv
	mu           sync.Mutex
	remaining    int
	attemptCount int
}

// newRetryEnv 装配一个前 remaining 次 prepare 事务注入序列化失败的环境。
//
// 只对 prepare_confirmation 生效：注入点若不限定操作，
// 连初始化库存这步都会被注入失败，测试就测不到真实的重试路径。
func newRetryEnv(t *testing.T, remaining int) *retryEnv {
	t.Helper()

	pool := pgtestDatabase(t)
	clock := &testClock{now: fixedNow}
	env := &retryEnv{testEnv: &testEnv{pool: pool, clock: clock}, remaining: remaining}

	store, err := pg.New(pg.Config{
		Pool:  pool,
		Clock: clock,
		FaultOperations: func(operation string) bool {
			return operation == "prepare_confirmation"
		},
		ObserveTransaction: func(operation string) {
			if operation != "prepare_confirmation" {
				return
			}
			// 事务开始的次数才是「尝试次数」。故障点位于提交之前，
			// 库存不足这类业务失败根本走不到那里，只数故障点会漏掉它。
			env.mu.Lock()
			env.attemptCount++
			env.mu.Unlock()
		},
		Faults: func(point pg.FaultPoint) error {
			if point != pg.FaultBeforeCommit {
				return nil
			}
			env.mu.Lock()
			defer env.mu.Unlock()
			if env.remaining > 0 {
				env.remaining--
				return pgConflictError()
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("装配存储失败：%v", err)
	}
	env.store = store
	return env
}

// attempts 返回事务尝试次数。
func (e *retryEnv) attempts() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.attemptCount
}

// pgConflictError 构造一个「序列化失败」错误。
//
// 刻意用真实的 pgconn.PgError 而不是自定义类型：重试判据读的是 SQLSTATE 码，
// 拿别的错误类型测出来的「能重试」在真实冲突下未必成立。
func pgConflictError() error {
	return &pgconn.PgError{
		Code:    "40001",
		Message: "could not serialize access due to concurrent update",
	}
}

// --- B6：栅栏拒绝迟到写 ---

func TestSessionClaimRejectsStaleWrites(t *testing.T) {
	env := newEnv(t)
	other := newStoreFor(t, env)
	ctx := context.Background()

	t.Run("新执行权让旧票据失效", func(t *testing.T) {
		first, err := env.store.Claim(ctx, "session-a", "buyer-1", true, true)
		if err != nil {
			t.Fatalf("第一次取执行权失败：%v", err)
		}
		if first.Fence != 1 {
			t.Errorf("首个 fence = %d，期望 1", first.Fence)
		}

		second, err := other.Claim(ctx, "session-a", "buyer-1", true, true)
		if err != nil {
			t.Fatalf("第二次取执行权失败：%v", err)
		}
		if second.Fence != 2 {
			t.Errorf("第二个 fence = %d，期望 2", second.Fence)
		}

		// 迟到的旧执行者拿第一次的票据写回，必须被拒绝
		_, err = env.store.Save(ctx, first, `{"value":1}`)
		if !errors.Is(err, session.ErrStaleWrite) {
			t.Fatalf("迟到写应返回 ErrStaleWrite，实际：%v", err)
		}

		// 当前执行者可以写
		if _, err := other.Save(ctx, second, `{"value":2}`); err != nil {
			t.Fatalf("当前执行者写入失败：%v", err)
		}
	})

	t.Run("保存后版本前进且旧版本被拒绝", func(t *testing.T) {
		claim, err := env.store.Claim(ctx, "session-b", "buyer-1", true, true)
		if err != nil {
			t.Fatalf("取执行权失败：%v", err)
		}

		saved, err := env.store.Save(ctx, claim, `{"value":1}`)
		if err != nil {
			t.Fatalf("保存失败：%v", err)
		}
		if saved.Revision != claim.Revision+1 {
			t.Errorf("保存后版本 = %d，期望 %d", saved.Revision, claim.Revision+1)
		}

		// 用同一张旧票据再写一次：版本已经前进，必须被拒绝
		if _, err := env.store.Save(ctx, claim, `{"value":2}`); !errors.Is(err, session.ErrStaleWrite) {
			t.Fatalf("重复保存应返回 ErrStaleWrite，实际：%v", err)
		}

		state, found, err := env.store.Load(ctx, "session-b")
		if err != nil || !found {
			t.Fatalf("读取快照失败：found=%v err=%v", found, err)
		}
		if state != `{"value":1}` {
			t.Errorf("快照 = %q，期望仍是第一次写入的内容", state)
		}
	})

	t.Run("并发取执行权得到唯一且递增的 fence", func(t *testing.T) {
		const concurrency = 8
		fences := make([]int64, concurrency)
		errs := make([]error, concurrency)

		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				claim, err := newStoreFor(t, env).Claim(ctx, "session-c", "buyer-1", true, true)
				fences[i], errs[i] = claim.Fence, err
			}()
		}
		close(start)
		wg.Wait()

		seen := make(map[int64]bool, concurrency)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("第 %d 个取执行权失败：%v", i, err)
			}
			if seen[fences[i]] {
				t.Fatalf("fence %d 重复分配", fences[i])
			}
			seen[fences[i]] = true
		}
		for want := int64(1); want <= concurrency; want++ {
			if !seen[want] {
				t.Errorf("缺少 fence %d，实际分配：%v", want, fences)
			}
		}
	})

	t.Run("其他买家不能取得或写入会话", func(t *testing.T) {
		if _, err := env.store.Claim(ctx, "session-d", "buyer-1", true, true); err != nil {
			t.Fatalf("取执行权失败：%v", err)
		}
		if _, err := other.Claim(ctx, "session-d", "buyer-2", true, true); !errors.Is(err, session.ErrOwnerMismatch) {
			t.Fatalf("其他买家应返回 ErrOwnerMismatch，实际：%v", err)
		}
		if err := other.AssertOwner(ctx, "session-d", "buyer-2", true, true); !errors.Is(err, session.ErrOwnerMismatch) {
			t.Fatalf("归属校验应返回 ErrOwnerMismatch，实际：%v", err)
		}
	})

	t.Run("归属校验不会推进执行权", func(t *testing.T) {
		// 读取会话（列表、概览）不应该把正在执行的写者作废
		before, err := env.store.Claim(ctx, "session-h", "buyer-1", true, true)
		if err != nil {
			t.Fatalf("取执行权失败：%v", err)
		}
		if err := env.store.AssertOwner(ctx, "session-h", "buyer-1", false, true); err != nil {
			t.Fatalf("归属校验失败：%v", err)
		}
		after, err := env.store.Claim(ctx, "session-h", "buyer-1", true, true)
		if err != nil {
			t.Fatalf("再次取执行权失败：%v", err)
		}
		if after.Fence != before.Fence+1 {
			t.Errorf("fence 由 %d 变成 %d，期望只前进一次（归属校验不占执行权）", before.Fence, after.Fence)
		}
	})

	t.Run("快照必须是 JSON 对象", func(t *testing.T) {
		claim, err := env.store.Claim(ctx, "session-e", "buyer-1", true, true)
		if err != nil {
			t.Fatalf("取执行权失败：%v", err)
		}
		for _, bad := range []string{"", "not json", "[1,2,3]", `"text"`, "42"} {
			if _, err := env.store.Save(ctx, claim, bad); !errors.Is(err, session.ErrCorruptState) {
				t.Errorf("快照 %q 应返回 ErrCorruptState，实际：%v", bad, err)
			}
		}
	})

	t.Run("快照可读回且不存在的会话返回未找到", func(t *testing.T) {
		claim, err := env.store.Claim(ctx, "session-f", "buyer-1", true, true)
		if err != nil {
			t.Fatalf("取执行权失败：%v", err)
		}
		if claim.HasState {
			t.Error("新会话不应带快照")
		}
		if _, err := env.store.Save(ctx, claim, `{"messages":["你好"]}`); err != nil {
			t.Fatalf("保存失败：%v", err)
		}

		state, found, err := env.store.Load(ctx, "session-f")
		if err != nil {
			t.Fatalf("读取失败：%v", err)
		}
		if !found || state != `{"messages":["你好"]}` {
			t.Errorf("读回快照 = %q，期望保存的内容", state)
		}

		if _, found, err := env.store.Load(ctx, "session-unknown"); err != nil || found {
			t.Errorf("不存在的会话应返回空且不报错，实际 found=%v err=%v", found, err)
		}

		if _, err := env.store.Claim(ctx, "session-unknown", "buyer-1", false, true); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("不存在且不允许创建时应返回 ErrNotFound，实际：%v", err)
		}
	})
}
