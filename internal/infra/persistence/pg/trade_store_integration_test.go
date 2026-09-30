package pg_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicoYazawa/crosspilot/internal/domain/catalog"
	"github.com/NicoYazawa/crosspilot/internal/domain/order"
	"github.com/NicoYazawa/crosspilot/internal/domain/trade"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg/pgtest"
)

// fixedNow 是测试的固定时钟起点。
//
// 用固定时钟而不是真实时间：确认单的过期判定、创建时间与摘要都依赖时间，
// 用真实时间会让「同一输入的两次运行」得到不同摘要，测试也就无法断言确定值。
var fixedNow = time.Date(2026, 9, 9, 8, 0, 0, 0, time.UTC)

// testClock 是可控时钟。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

// Now 返回当前测试时间。
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance 推进测试时间。
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Set 设置测试时间。
func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// testEnv 是一套独立的账本测试环境。
type testEnv struct {
	pool  *pgxpool.Pool
	store *pg.Store
	clock *testClock
}

// dbHelper 是测试与基准都需要的最小断言接口。
//
// 声明成本地接口而不是直接用 *testing.T，是为了让同一套夹具既能服务
// Test（断言失败即终止）也能服务 Benchmark（b.Fatalf 同样终止）。
type dbHelper interface {
	Helper()
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
	Skipf(format string, args ...any)
	Cleanup(func())
	Name() string
}

// newEnv 建一个独立库并装配存储。
func newEnv(t dbHelper) *testEnv {
	t.Helper()

	pool := pgtest.NewDatabase(t, func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pg.Migrate(ctx, pool, pg.Up)
		return err
	})

	clock := &testClock{now: fixedNow}
	store, err := pg.New(pg.Config{
		Pool:   pool,
		Clock:  clock,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
	})
	if err != nil {
		t.Fatalf("装配存储失败：%v", err)
	}
	return &testEnv{pool: pool, store: store, clock: clock}
}

// newBenchEnv 让基准用例复用同一套夹具。
func newBenchEnv(b *testing.B) *testEnv {
	b.Helper()
	return newEnv(b)
}

// seedProduct 灌入一份库存种子。
func (e *testEnv) seedProduct(t dbHelper, skuID, productID string, stock, price int64, currency catalog.Currency) {
	t.Helper()
	e.seedProducts(t, seedSpec(skuID, productID, stock, price, currency))
}

// seedSpec 构造一个库存种子。
func seedSpec(skuID, productID string, stock, price int64, currency catalog.Currency) trade.SeedSKU {
	return trade.SeedSKU{
		SKUID:          skuID,
		ProductID:      productID,
		Title:          "测试商品（标准款）",
		Stock:          stock,
		UnitPriceMinor: price,
		Currency:       currency,
	}
}

// seedProducts 灌入多个库存种子。
func (e *testEnv) seedProducts(t dbHelper, skus ...trade.SeedSKU) {
	t.Helper()
	if err := e.store.InitializeInventory(context.Background(), skus); err != nil {
		t.Fatalf("初始化库存失败：%v", err)
	}
}

// address 返回一份可用的收货地址。
func address() order.Address {
	return order.Address{
		Recipient:  "测试买家",
		Phone:      "13800000000",
		Country:    "CN",
		Province:   "上海",
		City:       "上海",
		Line1:      "测试路 1 号",
		PostalCode: "200000",
	}
}

// createRequest 构造一份下单请求。
func createRequest(operationID string, items ...trade.Item) trade.PrepareRequest {
	return trade.PrepareRequest{
		OperationID:     operationID,
		BuyerID:         "buyer-1",
		SessionID:       "session-1",
		Action:          trade.ActionCreate,
		Items:           items,
		ShippingAddress: address(),
		ExpiresAt:       fixedNow.Add(5 * time.Minute),
	}
}

// item 构造一行下单明细。
func item(skuID, productID string, quantity, price int64, currency catalog.Currency) trade.Item {
	return trade.Item{
		ProductID:      productID,
		SKUID:          skuID,
		Title:          "模型给出的标题",
		UnitPriceMinor: price,
		Currency:       currency,
		Quantity:       quantity,
	}
}

// defaultItem 是默认的一行明细：2 件 sku-1，单价 12900 CNY。
func defaultItem(quantity int64) trade.Item {
	return item("sku-1", "p-1", quantity, 12900, catalog.CNY)
}

// prepare 执行一次下单准备。
func (e *testEnv) prepare(t *testing.T, req trade.PrepareRequest) trade.Confirmation {
	t.Helper()
	c, err := e.store.Prepare(context.Background(), req)
	if err != nil {
		t.Fatalf("准备确认单失败：%v", err)
	}
	return c
}

// resolve 执行一次决议。
func (e *testEnv) resolve(t *testing.T, confirmation trade.Confirmation, decision trade.Decision) trade.Confirmation {
	t.Helper()
	resolved, err := e.store.Resolve(context.Background(),
		confirmation.ConfirmationID, confirmation.BuyerID, confirmation.SessionID,
		confirmation.SnapshotHash, decision)
	if err != nil {
		t.Fatalf("决议失败：%v", err)
	}
	return resolved
}

// inventory 读取全部库存。
func (e *testEnv) inventory(t *testing.T) map[string]int64 {
	t.Helper()
	stock, err := e.store.Inventory(context.Background(), nil)
	if err != nil {
		t.Fatalf("读取库存失败：%v", err)
	}
	return stock
}

// rowCounts 统计账本三张结果表的行数。
func (e *testEnv) rowCounts(t *testing.T) (orders, lines, operations int) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), `
SELECT
    (SELECT count(*) FROM orders),
    (SELECT count(*) FROM order_lines),
    (SELECT count(*) FROM trade_operations)`).Scan(&orders, &lines, &operations); err != nil {
		t.Fatalf("统计行数失败：%v", err)
	}
	return orders, lines, operations
}

// codeOf 取出错误的错误码。
func codeOf(t *testing.T, err error) string {
	t.Helper()
	var storeErr *trade.StoreError
	if !errors.As(err, &storeErr) {
		t.Fatalf("期望带错误码的拒绝错误，实际：%v", err)
	}
	return storeErr.Code
}

// assertCode 断言错误码。
func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，实际没有错误", want)
	}
	if got := codeOf(t, err); got != want {
		t.Fatalf("错误码 = %s，期望 %s（原始错误：%v）", got, want, err)
	}
}

// --- 基本两阶段提交 ---

func TestCreateCommitsOrderInventoryAndDecisionTogether(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)

	confirmation := env.prepare(t, createRequest("operation-1", defaultItem(2)))

	if confirmation.Status != trade.StatusPending {
		t.Errorf("新建确认单状态 = %s，期望 pending", confirmation.Status)
	}
	if confirmation.Payload.Create == nil {
		t.Fatal("确认单载荷应为下单载荷")
	}
	if got := confirmation.Payload.Create.TotalAmountMinor; got != 25800 {
		t.Errorf("总额 = %d，期望 25800", got)
	}
	if got := confirmation.Payload.Create.Items[0].Title; got != "测试商品（标准款）" {
		t.Errorf("标题 = %q，期望取自权威库存记录", got)
	}
	if got := env.inventory(t)["sku-1"]; got != 5 {
		t.Errorf("准备阶段不应扣减库存，实际 %d", got)
	}

	resolved := env.resolve(t, confirmation, trade.DecisionApprove)
	if resolved.Status != trade.StatusApproved {
		t.Errorf("决议后状态 = %s，期望 approved", resolved.Status)
	}
	if resolved.Result == nil {
		t.Fatal("批准后应带订单结果")
	}
	if resolved.Result.Status != order.StatusConfirmed {
		t.Errorf("订单状态 = %s，期望 CONFIRMED", resolved.Result.Status)
	}
	if len(resolved.Result.OrderID) != 32 {
		t.Errorf("订单标识长度 = %d，期望 32", len(resolved.Result.OrderID))
	}
	if resolved.Result.OrderKind != trade.OrderKind {
		t.Errorf("order_kind = %q，期望 %q", resolved.Result.OrderKind, trade.OrderKind)
	}
	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Errorf("决议后库存 = %d，期望 3", got)
	}

	first, lines, operations := env.rowCounts(t)
	if first != 1 || lines != 1 || operations != 1 {
		t.Errorf("行数 = (orders %d, lines %d, operations %d)，期望 (1, 1, 1)", first, lines, operations)
	}

	// 读回的订单必须与决议返回的结果逐字段一致：
	// 两处若不一致，说明「写进去的」与「告诉调用方的」是两份数据。
	stored, err := env.store.Order(context.Background(), resolved.Result.OrderID, "buyer-1")
	if err != nil {
		t.Fatalf("读取订单失败：%v", err)
	}
	if stored.TotalAmountMinor != resolved.Result.TotalAmountMinor ||
		stored.Status != resolved.Result.Status ||
		stored.Currency != resolved.Result.Currency {
		t.Errorf("读回订单与决议结果不一致：%+v / %+v", stored, *resolved.Result)
	}
	if len(stored.Lines) != 1 || stored.Lines[0].SKUID != "sku-1" || stored.Lines[0].Quantity != 2 {
		t.Errorf("读回订单明细不符：%+v", stored.Lines)
	}
}

func TestPrepareReplayReturnsExistingEvenWithNewExpiry(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)

	original := env.prepare(t, createRequest("operation-1", defaultItem(2)))

	replay := createRequest("operation-1", defaultItem(2))
	replay.ExpiresAt = fixedNow.Add(10 * time.Minute)
	got := env.prepare(t, replay)

	if got.ConfirmationID != original.ConfirmationID {
		t.Errorf("重放返回了新确认单 %s，期望复用 %s", got.ConfirmationID, original.ConfirmationID)
	}
	if !got.ExpiresAt.Equal(original.ExpiresAt) {
		t.Errorf("重放有效期 = %s，期望保持原值 %s", got.ExpiresAt, original.ExpiresAt)
	}

	env.resolve(t, original, trade.DecisionApprove)
	after := env.prepare(t, createRequest("operation-1", defaultItem(2)))
	if after.Status != trade.StatusApproved {
		t.Errorf("已决议确认单重放状态 = %s，期望 approved", after.Status)
	}

	orders, _, _ := env.rowCounts(t)
	if orders != 1 {
		t.Errorf("重放不应产生第二笔订单，实际 %d 笔", orders)
	}
}

func TestOperationIDCannotChangeBoundContent(t *testing.T) {
	cases := []struct {
		name  string
		items []trade.Item
	}{
		{"数量变化", []trade.Item{defaultItem(3)}},
		{"单价变化", []trade.Item{item("sku-1", "p-1", 2, 100, catalog.CNY)}},
		{"币种变化", []trade.Item{item("sku-1", "p-1", 2, 12900, catalog.USD)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newEnv(t)
			env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
			env.prepare(t, createRequest("operation-1", defaultItem(2)))

			_, err := env.store.Prepare(context.Background(), createRequest("operation-1", tc.items...))
			assertCode(t, err, trade.CodeOperationConflict)
		})
	}

	t.Run("地址变化", func(t *testing.T) {
		env := newEnv(t)
		env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
		env.prepare(t, createRequest("operation-1", defaultItem(2)))

		changed := createRequest("operation-1", defaultItem(2))
		changed.ShippingAddress.Line1 = "另一条路 2 号"
		_, err := env.store.Prepare(context.Background(), changed)
		assertCode(t, err, trade.CodeOperationConflict)
	})
}

func TestConfirmationOwnerIsEnforcedEverywhere(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	confirmation := env.prepare(t, createRequest("operation-1", defaultItem(2)))

	t.Run("换买家准备", func(t *testing.T) {
		req := createRequest("operation-1", defaultItem(2))
		req.BuyerID = "buyer-2"
		_, err := env.store.Prepare(context.Background(), req)
		assertCode(t, err, trade.CodeOwnerMismatch)
	})

	t.Run("换会话准备", func(t *testing.T) {
		req := createRequest("operation-1", defaultItem(2))
		req.SessionID = "session-2"
		_, err := env.store.Prepare(context.Background(), req)
		assertCode(t, err, trade.CodeOwnerMismatch)
	})

	t.Run("换买家决议", func(t *testing.T) {
		_, err := env.store.Resolve(context.Background(),
			confirmation.ConfirmationID, "buyer-2", confirmation.SessionID,
			confirmation.SnapshotHash, trade.DecisionApprove)
		assertCode(t, err, trade.CodeOwnerMismatch)
	})

	t.Run("换会话决议", func(t *testing.T) {
		_, err := env.store.Resolve(context.Background(),
			confirmation.ConfirmationID, confirmation.BuyerID, "session-2",
			confirmation.SnapshotHash, trade.DecisionApprove)
		assertCode(t, err, trade.CodeOwnerMismatch)
	})

	t.Run("换买家读取", func(t *testing.T) {
		_, err := env.store.Confirmation(context.Background(),
			confirmation.ConfirmationID, "buyer-2", confirmation.SessionID)
		assertCode(t, err, trade.CodeOwnerMismatch)
	})

	t.Run("换买家列表返回空而不是报错", func(t *testing.T) {
		list, err := env.store.Confirmations(context.Background(), "buyer-2", confirmation.SessionID, 20)
		if err != nil {
			t.Fatalf("列表不应报错：%v", err)
		}
		if len(list) != 0 {
			t.Errorf("其他买家的列表应为空，实际 %d 条", len(list))
		}
	})
}

func TestHashAndDecisionReplayAreEnforced(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	confirmation := env.prepare(t, createRequest("operation-1", defaultItem(2)))

	t.Run("错误的快照摘要被拒绝", func(t *testing.T) {
		_, err := env.store.Resolve(context.Background(),
			confirmation.ConfirmationID, confirmation.BuyerID, confirmation.SessionID,
			"0000000000000000000000000000000000000000000000000000000000000000",
			trade.DecisionApprove)
		assertCode(t, err, trade.CodeSnapshotMismatch)
	})

	t.Run("形状不对的摘要被拒绝", func(t *testing.T) {
		_, err := env.store.Resolve(context.Background(),
			confirmation.ConfirmationID, confirmation.BuyerID, confirmation.SessionID,
			"tampered-hash", trade.DecisionApprove)
		assertCode(t, err, trade.CodeSnapshotMismatch)
	})

	t.Run("拒绝幂等且不扣库存", func(t *testing.T) {
		rejected := env.resolve(t, confirmation, trade.DecisionReject)
		if rejected.Status != trade.StatusRejected {
			t.Errorf("状态 = %s，期望 rejected", rejected.Status)
		}
		if rejected.Result != nil {
			t.Errorf("拒绝不应带订单结果，实际 %+v", rejected.Result)
		}

		again := env.resolve(t, confirmation, trade.DecisionReject)
		if again.Status != trade.StatusRejected || again.ResolvedAt == nil {
			t.Errorf("重复拒绝应为幂等，实际 %+v", again)
		}

		_, err := env.store.Resolve(context.Background(),
			confirmation.ConfirmationID, confirmation.BuyerID, confirmation.SessionID,
			confirmation.SnapshotHash, trade.DecisionApprove)
		assertCode(t, err, trade.CodeDecisionConflict)

		if got := env.inventory(t)["sku-1"]; got != 5 {
			t.Errorf("拒绝后库存 = %d，期望 5", got)
		}
		orders, _, operations := env.rowCounts(t)
		if orders != 0 || operations != 1 {
			t.Errorf("拒绝应只写一条决议记录，实际 orders=%d operations=%d", orders, operations)
		}
	})
}

func TestPendingExpiresButCommittedReplayRemainsValid(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	confirmation := env.prepare(t, createRequest("operation-1", defaultItem(2)))

	// 先完成一笔，再跳到一天后
	committed := env.resolve(t, confirmation, trade.DecisionApprove)
	env.clock.Set(fixedNow.Add(24 * time.Hour))

	t.Run("已决议的确认单在过期后仍幂等返回", func(t *testing.T) {
		again, err := env.store.Resolve(context.Background(),
			confirmation.ConfirmationID, confirmation.BuyerID, confirmation.SessionID,
			confirmation.SnapshotHash, trade.DecisionApprove)
		if err != nil {
			t.Fatalf("幂等重放不应报错：%v", err)
		}
		if again.Status != trade.StatusApproved {
			t.Errorf("重放状态 = %s，期望 approved", again.Status)
		}
		if again.Result == nil || again.Result.OrderID != committed.Result.OrderID {
			t.Errorf("重放应返回首次结果，实际 %+v", again.Result)
		}
	})

	t.Run("未决议的确认单在过期后不能被决议", func(t *testing.T) {
		// 时钟先回到起点，准备一张短暂有效的确认单
		env.clock.Set(fixedNow)
		pending := env.prepare(t, createRequest("operation-2", defaultItem(1)))

		env.clock.Set(fixedNow.Add(24 * time.Hour))
		_, err := env.store.Resolve(context.Background(),
			pending.ConfirmationID, pending.BuyerID, pending.SessionID,
			pending.SnapshotHash, trade.DecisionApprove)
		assertCode(t, err, trade.CodeConfirmationExpired)

		// 即使是拒绝决议也必须过期：过期是「这份价格不再可信」，
		// 与用户点的是同意还是拒绝无关
		_, err = env.store.Resolve(context.Background(),
			pending.ConfirmationID, pending.BuyerID, pending.SessionID,
			pending.SnapshotHash, trade.DecisionReject)
		assertCode(t, err, trade.CodeConfirmationExpired)
	})

	t.Run("prepare 拒绝已过期的有效期", func(t *testing.T) {
		env.clock.Set(fixedNow)
		req := createRequest("operation-3", defaultItem(1))
		req.ExpiresAt = fixedNow.Add(-time.Minute)
		_, err := env.store.Prepare(context.Background(), req)
		assertCode(t, err, trade.CodeConfirmationExpired)
	})

	t.Run("决议时才过期的确认单被拒绝", func(t *testing.T) {
		// 用当前时钟准备一张有效期很短的确认单，再推进时钟越过它
		env.clock.Set(fixedNow)
		req := createRequest("operation-4", defaultItem(1))
		req.ExpiresAt = fixedNow.Add(2 * time.Minute)
		shortLived := env.prepare(t, req)

		env.clock.Set(fixedNow.Add(3 * time.Minute))
		_, err := env.store.Resolve(context.Background(),
			shortLived.ConfirmationID, shortLived.BuyerID, shortLived.SessionID,
			shortLived.SnapshotHash, trade.DecisionApprove)
		assertCode(t, err, trade.CodeConfirmationExpired)

		// 过期确认单在读取时应当被标记为已过期
		read, err := env.store.Confirmation(context.Background(),
			shortLived.ConfirmationID, shortLived.BuyerID, shortLived.SessionID)
		if err != nil {
			t.Fatalf("读取确认单失败：%v", err)
		}
		if !read.Expired(env.clock.Now()) {
			t.Error("确认单应被标记为已过期")
		}
	})
}

// --- 无关连接上的幂等与竞争 ---

func TestIndependentConnectionsReplayOneDecisionOnce(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)
	confirmation := env.prepare(t, createRequest("operation-1", defaultItem(2)))

	// 两个 store 共用一个池但走各自的事务，模拟两个请求同时决议
	second := newStoreFor(t, env)

	results, errs := resolveConcurrently(t,
		resolveCall{store: env.store, confirmation: confirmation},
		resolveCall{store: second, confirmation: confirmation},
	)

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个决议失败：%v", i, err)
		}
	}
	if results[0].Result == nil || results[1].Result == nil {
		t.Fatalf("两个决议都应返回订单结果：%+v", results)
	}
	if results[0].Result.OrderID != results[1].Result.OrderID {
		t.Errorf("并发决议返回了不同订单：%s / %s", results[0].Result.OrderID, results[1].Result.OrderID)
	}
	if got := env.inventory(t)["sku-1"]; got != 3 {
		t.Errorf("库存 = %d，期望只扣减一次（3）", got)
	}
	orders, lines, operations := env.rowCounts(t)
	if orders != 1 || lines != 1 || operations != 1 {
		t.Errorf("行数 = (%d, %d, %d)，期望 (1, 1, 1)", orders, lines, operations)
	}
}

func TestIndependentOperationsRaceForLastStock(t *testing.T) {
	env := newEnv(t)
	env.seedProduct(t, "sku-1", "p-1", 5, 12900, catalog.CNY)

	first := env.prepare(t, createRequest("operation-1", defaultItem(4)))
	second := env.prepare(t, createRequest("operation-2", defaultItem(4)))
	other := newStoreFor(t, env)

	results, errs := resolveConcurrently(t,
		resolveCall{store: env.store, confirmation: first},
		resolveCall{store: other, confirmation: second},
	)

	var succeeded, insufficient int
	for i, err := range errs {
		switch {
		case err == nil:
			succeeded++
			if results[i].Result == nil {
				t.Error("成功的一方应返回订单结果")
			}
		case codeOf(t, err) == trade.CodeInsufficientStock:
			insufficient++
		default:
			t.Fatalf("第 %d 个决议返回意外错误：%v", i, err)
		}
	}
	if succeeded != 1 || insufficient != 1 {
		t.Errorf("成功 %d 笔、库存不足 %d 笔，期望恰好各 1", succeeded, insufficient)
	}
	if got := env.inventory(t)["sku-1"]; got != 1 {
		t.Errorf("库存 = %d，期望 1", got)
	}
	orders, lines, operations := env.rowCounts(t)
	if orders != 1 || lines != 1 || operations != 1 {
		t.Errorf("行数 = (%d, %d, %d)，期望 (1, 1, 1)", orders, lines, operations)
	}
}

// newStoreFor 用同一个池构造第二个存储实例。
//
// 两个实例共享连接池但不共享任何内存状态：并发正确性因此只能来自数据库，
// 而不是来自应用层某把恰好生效的进程内锁。
func newStoreFor(t *testing.T, env *testEnv) *pg.Store {
	t.Helper()
	store, err := pg.New(pg.Config{Pool: env.pool, Clock: env.clock})
	if err != nil {
		t.Fatalf("构造第二个存储实例失败：%v", err)
	}
	return store
}

// resolveCall 是一次并发决议。
type resolveCall struct {
	store        *pg.Store
	confirmation trade.Confirmation
}

// resolveConcurrently 并发执行多次决议并收集结果。
func resolveConcurrently(t *testing.T, calls ...resolveCall) ([]trade.Confirmation, []error) {
	t.Helper()

	results := make([]trade.Confirmation, len(calls))
	errs := make([]error, len(calls))

	var wg sync.WaitGroup
	start := make(chan struct{})
	// 取元素地址而不拷贝整个 resolveCall（含 Confirmation，216 字节）；
	// 每个 goroutine 都要拿到自己那一轮的 i 与 call，故显式传参。
	for i := range calls {
		call := &calls[i]
		wg.Add(1)
		go func(i int, call *resolveCall) {
			defer wg.Done()
			<-start
			results[i], errs[i] = call.store.Resolve(context.Background(),
				call.confirmation.ConfirmationID, call.confirmation.BuyerID, call.confirmation.SessionID,
				call.confirmation.SnapshotHash, trade.DecisionApprove)
		}(i, call)
	}
	close(start)
	wg.Wait()
	return results, errs
}

// --- B1：并发幂等 ---

func TestConcurrentSameOperationCreatesExactlyOneTrade(t *testing.T) {
	env := newEnv(t)
	const concurrency = 32

	// 库存足够，让所有请求都走完流程，从而真正考验幂等而不是库存拒绝
	env.seedProduct(t, "sku-1", "p-1", concurrency*4, 12900, catalog.CNY)

	stores := make([]*pg.Store, concurrency)
	for i := range stores {
		stores[i] = newStoreFor(t, env)
	}

	confirmations := make([]trade.Confirmation, concurrency)
	errs := make([]error, concurrency)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			confirmations[i], errs[i] = stores[i].Prepare(context.Background(),
				createRequest("operation-shared", defaultItem(2)))
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("第 %d 个并发请求失败：%v", i, err)
		}
	}

	// 全部返回同一张确认单
	for i, c := range confirmations {
		if c.ConfirmationID != confirmations[0].ConfirmationID {
			t.Fatalf("第 %d 个请求拿到不同确认单：%s vs %s", i, c.ConfirmationID, confirmations[0].ConfirmationID)
		}
	}

	var stored int
	if err := env.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM trade_confirmations WHERE operation_id = 'operation-shared'`).Scan(&stored); err != nil {
		t.Fatalf("统计确认单失败：%v", err)
	}
	if stored != 1 {
		t.Fatalf("同一操作编号下有 %d 张确认单，期望恰好 1 张", stored)
	}

	env.resolve(t, confirmations[0], trade.DecisionApprove)
	orders, lines, operations := env.rowCounts(t)
	if orders != 1 || lines != 1 || operations != 1 {
		t.Errorf("行数 = (%d, %d, %d)，期望恰好 1 笔交易", orders, lines, operations)
	}
	if got := env.inventory(t)["sku-1"]; got != concurrency*4-2 {
		t.Errorf("库存 = %d，期望只扣一次（%d）", got, concurrency*4-2)
	}
}

// --- B2：库存不超卖 ---

func TestConcurrentBuyersCannotOversell(t *testing.T) {
	env := newEnv(t)
	const (
		stock       = 10
		concurrency = 100
	)
	env.seedProduct(t, "sku-1", "p-1", stock, 12900, catalog.CNY)

	// 每个买家一张确认单，各自抢最后 10 件
	confirmations := make([]trade.Confirmation, concurrency)
	for i := 0; i < concurrency; i++ {
		confirmations[i] = env.prepare(t, trade.PrepareRequest{
			OperationID:     fmt.Sprintf("operation-%d", i),
			BuyerID:         fmt.Sprintf("buyer-%d", i),
			SessionID:       fmt.Sprintf("session-%d", i),
			Action:          trade.ActionCreate,
			Items:           []trade.Item{defaultItem(1)},
			ShippingAddress: address(),
			ExpiresAt:       fixedNow.Add(5 * time.Minute),
		})
	}

	var (
		wg         sync.WaitGroup
		start      = make(chan struct{})
		mu         sync.Mutex
		succeeded  int
		rejected   int
		unexpected []error
	)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := env.store.Resolve(context.Background(),
				confirmations[i].ConfirmationID, confirmations[i].BuyerID, confirmations[i].SessionID,
				confirmations[i].SnapshotHash, trade.DecisionApprove)

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				succeeded++
			case codeOf(t, err) == trade.CodeInsufficientStock:
				rejected++
			default:
				unexpected = append(unexpected, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range unexpected {
		t.Errorf("意外错误：%v", err)
	}
	if succeeded != stock {
		t.Errorf("成功 %d 笔，期望恰好 %d 笔", succeeded, stock)
	}
	if rejected != concurrency-stock {
		t.Errorf("库存不足 %d 笔，期望 %d 笔", rejected, concurrency-stock)
	}
	if got := env.inventory(t)["sku-1"]; got != 0 {
		t.Errorf("库存 = %d，期望 0", got)
	}

	orders, _, _ := env.rowCounts(t)
	if orders != stock {
		t.Errorf("订单数 = %d，期望 %d", orders, stock)
	}
}
