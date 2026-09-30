package pg_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg/pgtest"
)

// ledgerTables 是 0002_trade_ledger 建出来的全部表。
//
// 把清单写死在这里而不是查 information_schema：查出来再比对等于
// 用「数据库里有什么」回答「数据库里该有什么」，那样漏建一张表也看不出来。
var ledgerTables = []string{
	"trade_sku_inventory",
	"trade_confirmations",
	"orders",
	"order_lines",
	"trade_operations",
	"session_write_claims",
	"agent_session_states",
}

// TestTradeLedgerMigrationAppliesAndRollsBack 验证新迁移的两个方向。
//
// 回滚脚本只有在真跑一次的时候才会暴露错误——它通常没人执行，
// 于是拼写错误、表名写错、外键顺序反了都会一直躺着，
// 直到某天真的需要回滚。这正是「必须实测」的典型场景。
func TestTradeLedgerMigrationAppliesAndRollsBack(t *testing.T) {
	// 这个库刻意不跑迁移：本用例要自己控制迁移的每一个方向
	pool := pgtest.NewDatabase(t, func(context.Context, *pgxpool.Pool) error { return nil })
	ctx := context.Background()

	missing := func() []string {
		var out []string
		for _, table := range ledgerTables {
			var exists bool
			err := pool.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM information_schema.tables
				 WHERE table_schema = 'public' AND table_name = $1)`, table).Scan(&exists)
			if err != nil {
				t.Fatalf("探测表 %s 失败：%v", table, err)
			}
			if !exists {
				out = append(out, table)
			}
		}
		return out
	}

	if got := missing(); len(got) != len(ledgerTables) {
		t.Fatalf("迁移前不应存在账本表，实际缺失 %d 张、共 %d 张", len(got), len(ledgerTables))
	}

	// --- up ---
	applied, err := pg.Migrate(ctx, pool, pg.Up)
	if err != nil {
		t.Fatalf("执行 up 迁移失败：%v", err)
	}
	if applied != 2 {
		t.Errorf("up 执行步数 = %d，期望 2（0001 与 0002）", applied)
	}
	if got := missing(); len(got) != 0 {
		t.Fatalf("up 之后仍缺少账本表：%v", got)
	}

	// --- up 幂等 ---
	again, err := pg.Migrate(ctx, pool, pg.Up)
	if err != nil {
		t.Fatalf("重复执行 up 失败：%v", err)
	}
	if again != 0 {
		t.Errorf("重复 up 执行步数 = %d，期望 0（已应用的迁移不应重放）", again)
	}

	// --- down 一步：回滚 0002，账本表应全部消失 ---
	rolledBack, err := pg.Migrate(ctx, pool, pg.Down)
	if err != nil {
		t.Fatalf("执行 down 迁移失败：%v", err)
	}
	if rolledBack != 1 {
		t.Errorf("down 执行步数 = %d，期望 1", rolledBack)
	}
	if got := missing(); len(got) != len(ledgerTables) {
		t.Fatalf("down 之后账本表应全部删除，实际仍存在 %d 张", len(ledgerTables)-len(got))
	}

	// --- 再 up：回滚之后必须还能重新升上来 ---
	reapplied, err := pg.Migrate(ctx, pool, pg.Up)
	if err != nil {
		t.Fatalf("回滚后重新执行 up 失败：%v", err)
	}
	if reapplied != 1 {
		t.Errorf("重新 up 执行步数 = %d，期望 1", reapplied)
	}
	if got := missing(); len(got) != 0 {
		t.Fatalf("重新 up 之后仍缺少账本表：%v", got)
	}
}

// TestTradeLedgerConstraintsAreEnforcedByDatabase 验证关键约束确实落在数据库里。
//
// 应用层的校验是主要防线，但约束是最后一道：它保证「即使有人绕过应用层
// 直连数据库」也不会写出负库存或没有理由的已取消订单。
// 若这些约束只写在应用层，那么一次误操作的数据修复就可能悄悄破坏账本。
func TestTradeLedgerConstraintsAreEnforcedByDatabase(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	cases := []struct {
		name string
		sql  string
	}{
		{
			name: "库存不能为负",
			sql: `INSERT INTO trade_sku_inventory (sku_id, product_id, title, stock, unit_price_minor, currency)
			      VALUES ('neg', 'p', 't', -1, 100, 'CNY')`,
		},
		{
			name: "单价不能为负",
			sql: `INSERT INTO trade_sku_inventory (sku_id, product_id, title, stock, unit_price_minor, currency)
			      VALUES ('negp', 'p', 't', 1, -1, 'CNY')`,
		},
		{
			name: "币种形状必须正确",
			sql: `INSERT INTO trade_sku_inventory (sku_id, product_id, title, stock, unit_price_minor, currency)
			      VALUES ('cur', 'p', 't', 1, 100, 'cny')`,
		},
		{
			name: "确认单状态必须在枚举内",
			sql: `INSERT INTO trade_confirmations (confirmation_id, operation_id, buyer_id, session_id, action,
			          request_hash, payload, snapshot_hash, expires_at, status, created_at)
			      VALUES ('c1', 'o1', 'b', 's', 'create',
			          repeat('0',64), '{}', repeat('0',64), now(), 'maybe', now())`,
		},
		{
			name: "确认单动作必须在枚举内",
			sql: `INSERT INTO trade_confirmations (confirmation_id, operation_id, buyer_id, session_id, action,
			          request_hash, payload, snapshot_hash, expires_at, status, created_at)
			      VALUES ('c2', 'o2', 'b', 's', 'refund',
			          repeat('0',64), '{}', repeat('0',64), now(), 'pending', now())`,
		},
		{
			name: "摘要必须是 64 位小写十六进制",
			sql: `INSERT INTO trade_confirmations (confirmation_id, operation_id, buyer_id, session_id, action,
			          request_hash, payload, snapshot_hash, expires_at, status, created_at)
			      VALUES ('c3', 'o3', 'b', 's', 'create',
			          'short', '{}', repeat('0',64), now(), 'pending', now())`,
		},
		{
			name: "订单状态必须在枚举内",
			sql: `INSERT INTO orders (order_id, buyer_id, status, currency, total_amount_minor,
			          shipping_address_json, created_at)
			      VALUES ('o', 'b', 'PAID', 'CNY', 1, '{}', now())`,
		},
		{
			name: "已取消订单必须有取消时间与原因",
			sql: `INSERT INTO orders (order_id, buyer_id, status, currency, total_amount_minor,
			          shipping_address_json, created_at)
			      VALUES ('o2', 'b', 'CANCELLED', 'CNY', 1, '{}', now())`,
		},
		{
			name: "订单行数量必须为正",
			sql: `INSERT INTO orders (order_id, buyer_id, status, currency, total_amount_minor,
			          shipping_address_json, created_at)
			      VALUES ('o3', 'b', 'CONFIRMED', 'CNY', 1, '{}', now());
			      INSERT INTO order_lines (order_id, sku_id, product_id, title, unit_price_minor, currency, quantity)
			      VALUES ('o3', 's', 'p', 't', 1, 'CNY', 0)`,
		},
		{
			name: "会话快照必须是 JSON 对象",
			sql:  `INSERT INTO agent_session_states (session_id, state_json) VALUES ('s1', '[1,2,3]')`,
		},
		{
			name: "会话 fence 不能为负",
			sql:  `INSERT INTO session_write_claims (session_id, owner_id, revision, fence) VALUES ('s2', 'b', 0, -1)`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := env.pool.Exec(ctx, tc.sql); err == nil {
				t.Fatal("数据库应当拒绝这条写入，实际成功——约束缺失或写错了")
			}
		})
	}
}
