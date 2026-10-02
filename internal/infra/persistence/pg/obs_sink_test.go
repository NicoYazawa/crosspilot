package pg_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	domainobs "github.com/NicoYazawa/crosspilot/internal/domain/observability"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg/pgtest"
)

// obsMigratedPool 建一个跑完全部迁移的独立库，供 P5 观测适配器的测试复用。
func obsMigratedPool(t dbHelper) *pgxpool.Pool {
	t.Helper()
	return pgtest.NewDatabase(t, func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := pg.Migrate(ctx, pool, pg.Up)
		return err
	})
}

// TestObsSinkAppendBatchAndIdempotency 覆盖：全新 run 的外键顺序、跨批 seq 递增、
// 以及重放同一批时的幂等（不报错、不写重行）。
func TestObsSinkAppendBatchAndIdempotency(t *testing.T) {
	pool := obsMigratedPool(t)
	ctx := context.Background()
	sink := pg.NewObsSink(pool, nil)

	// 一批里出现一个数据库里还不存在的 run_id：外键要求先有 run 行，
	// 这正是 Append 里「先补 run 再写 event」那段逻辑存在的理由。
	first := []domainobs.SinkRecord{
		{
			EventID: "evt-1", RunID: "run-obs-1", Seq: 1, Kind: "run.started", Agent: "main",
			PayloadSHA256: "sha-1", PayloadSize: 42, PayloadRedacted: []byte(`{"k":"v"}`),
			CreatedAt: 1_700_000_000,
		},
		{
			EventID: "evt-2", RunID: "run-obs-1", Seq: 2, Kind: "tool.call", Agent: "shopping",
			PayloadSHA256: "sha-2", PayloadSize: 7, PayloadRedacted: nil, // 空载荷 → 落 {}
			CreatedAt: 1_700_000_001,
		},
	}
	if err := sink.Append(ctx, first); err != nil {
		t.Fatalf("首批写入失败（外键顺序问题）：%v", err)
	}

	second := []domainobs.SinkRecord{
		{
			EventID: "evt-3", RunID: "run-obs-1", Seq: 3, Kind: "run.finished", Agent: "main",
			PayloadSHA256: "sha-3", PayloadSize: 3, PayloadRedacted: []byte(`{}`),
			CreatedAt: 1_700_000_002,
		},
	}
	if err := sink.Append(ctx, second); err != nil {
		t.Fatalf("第二批写入失败：%v", err)
	}

	// 重放首批：event_id 幂等，不应报错也不应产生重复行。
	if err := sink.Append(ctx, first); err != nil {
		t.Fatalf("重放同一批失败（应幂等）：%v", err)
	}

	var count int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM observability.event WHERE run_id = $1`, "run-obs-1").Scan(&count); err != nil {
		t.Fatalf("统计事件行失败：%v", err)
	}
	if count != 3 {
		t.Fatalf("事件行数 = %d，期望 3（重放不应写重）", count)
	}

	// 逐行核对 sha256 / size / seq。
	cases := []struct {
		eventID string
		seq     int64
		sha     string
		size    int
	}{
		{"evt-1", 1, "sha-1", 42},
		{"evt-2", 2, "sha-2", 7},
		{"evt-3", 3, "sha-3", 3},
	}
	for _, tc := range cases {
		var seq int64
		var sha string
		var size int
		err := pool.QueryRow(ctx,
			`SELECT seq, payload_sha256, payload_size FROM observability.event WHERE event_id = $1`,
			tc.eventID).Scan(&seq, &sha, &size)
		if err != nil {
			t.Fatalf("读回 %s 失败：%v", tc.eventID, err)
		}
		if seq != tc.seq || sha != tc.sha || size != tc.size {
			t.Errorf("%s = (seq=%d, sha=%s, size=%d)，期望 (%d, %s, %d)",
				tc.eventID, seq, sha, size, tc.seq, tc.sha, tc.size)
		}
	}

	// 空载荷必须落成 '{}' 而非 NULL。
	var payload string
	if err := pool.QueryRow(ctx,
		`SELECT payload_redacted::text FROM observability.event WHERE event_id = 'evt-2'`).Scan(&payload); err != nil {
		t.Fatalf("读回 payload_redacted 失败：%v", err)
	}
	if payload != "{}" {
		t.Errorf("空载荷落库 = %q，期望 {} ", payload)
	}

	if err := sink.Close(); err != nil {
		t.Errorf("Close 应为空操作返回 nil，实际：%v", err)
	}
}

// TestObsSinkSeqConflictSurfaces 钉住：两个不同 event_id 抢同一个 (run_id, seq)
// 是真实冲突，必须冒错，而不是被幂等逻辑吞掉。
func TestObsSinkSeqConflictSurfaces(t *testing.T) {
	pool := obsMigratedPool(t)
	ctx := context.Background()
	sink := pg.NewObsSink(pool, nil)

	base := domainobs.SinkRecord{
		EventID: "conflict-a", RunID: "run-conflict", Seq: 1, Kind: "run.started",
		PayloadSHA256: "s", PayloadSize: 1, PayloadRedacted: []byte(`{}`), CreatedAt: 1_700_000_000,
	}
	if err := sink.Append(ctx, []domainobs.SinkRecord{base}); err != nil {
		t.Fatalf("基线写入失败：%v", err)
	}

	clash := base
	clash.EventID = "conflict-b" // 不同 event_id，同一 (run_id, seq)
	if err := sink.Append(ctx, []domainobs.SinkRecord{clash}); err == nil {
		t.Fatal("同 (run_id, seq) 的不同 event_id 应当报唯一约束冲突，实际成功")
	}
}

// costRec 造一条带成本的事件记录。
func costRec(eventID string, seq int64, c *domainobs.CostEvent) domainobs.SinkRecord {
	return domainobs.SinkRecord{
		EventID: eventID, RunID: "run-cost-1", Seq: seq, Kind: "model_turn", Agent: "main",
		PayloadSHA256: "sha-" + eventID, PayloadSize: 10,
		PayloadRedacted: []byte(`{"usage":{"input_tokens":3600}}`),
		CreatedAt:       1_700_000_000,
		Cost:            c,
	}
}

// TestObsSinkWritesCostInSameTransaction 是「可观测真的通电了」的库级证据。
//
// 覆盖三件必须同时成立的事：
//  1. cost 行真的写进去了（写入路径存在，不只是有表）
//  2. 外键顺序正确——cost.event_id 指向 event.event_id，先写事件再写成本；
//     顺序反了会撞外键，而错误信息不会告诉你是顺序问题
//  3. unpriced 行的 priced_at 为 NULL、已定价行非 NULL：看板靠它算定价覆盖率
func TestObsSinkWritesCostInSameTransaction(t *testing.T) {
	pool := obsMigratedPool(t)
	ctx := context.Background()
	sink := pg.NewObsSink(pool, nil)

	priced := &domainobs.CostEvent{
		EventID: "evt-cost-1", RunID: "run-cost-1",
		Provider: "deepseek", Model: "deepseek-flash",
		TokensIn: 3600, TokensOut: 800, TokensCached: 200, TokensReasoning: 64,
		CostMinor: 13208, Currency: "CNY", Unpriced: false,
	}
	unpriced := &domainobs.CostEvent{
		EventID: "evt-cost-2", RunID: "run-cost-1",
		Provider: "deepseek", Model: "deepseek-chat",
		TokensIn: 100, TokensOut: 50,
		CostMinor: 0, Currency: "CNY", Unpriced: true,
	}

	batch := []domainobs.SinkRecord{
		costRec("evt-cost-1", 1, priced),
		costRec("evt-cost-2", 2, unpriced),
		// 非模型事件：不产生成本行。若这里也写一行全 0，成本看板的 total_calls
		// 会虚高，且「不涉及计费」与「花了 0 元」在表里长得一样。
		{
			EventID: "evt-cost-3", RunID: "run-cost-1", Seq: 3, Kind: "tool_result",
			PayloadSHA256: "sha-3", PayloadSize: 5, PayloadRedacted: []byte(`{}`),
			CreatedAt: 1_700_000_000,
		},
	}
	if err := sink.Append(ctx, batch); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	// cost 表不存 run_id，只能按 event_id 数（要按 run 聚合得 join event）。
	const countSQL = `SELECT COUNT(*) FROM observability.cost
		WHERE event_id IN ('evt-cost-1','evt-cost-2','evt-cost-3')`
	var rows int
	if err := pool.QueryRow(ctx, countSQL).Scan(&rows); err != nil {
		t.Fatalf("统计成本行失败：%v", err)
	}
	if rows != 2 {
		t.Fatalf("成本行数 = %d，期望 2（非模型事件不该有成本行）", rows)
	}

	// 逐列核对已定价行
	var (
		provider, model, currency              string
		tin, tout, tcached, treason, costMinor int64
		unpricedFlag                           bool
		pricedAt                               *time.Time
	)
	err := pool.QueryRow(ctx, `
		SELECT provider, model, tokens_in, tokens_out, tokens_cached, tokens_reasoning,
		       cost_minor, currency, unpriced, priced_at
		FROM observability.cost WHERE event_id = 'evt-cost-1'`).
		Scan(&provider, &model, &tin, &tout, &tcached, &treason, &costMinor, &currency, &unpricedFlag, &pricedAt)
	if err != nil {
		t.Fatalf("读回已定价行失败：%v", err)
	}
	if provider != "deepseek" || model != "deepseek-flash" {
		t.Errorf("身份 = %s/%s", provider, model)
	}
	if tin != 3600 || tout != 800 || tcached != 200 || treason != 64 {
		t.Errorf("token 计量 = (%d,%d,%d,%d)，期望 (3600,800,200,64)", tin, tout, tcached, treason)
	}
	if costMinor != 13208 {
		t.Errorf("cost_minor = %d，期望 13208", costMinor)
	}
	if currency != "CNY" {
		t.Errorf("currency = %q，期望 CNY", currency)
	}
	if unpricedFlag {
		t.Error("不该标 unpriced")
	}
	if pricedAt == nil {
		t.Error("已定价行的 priced_at 不该为 NULL——看板靠它算定价覆盖率")
	}

	// unpriced 行：费用 0 且 priced_at 为 NULL
	var unpricedCost int64
	var unpricedAt *time.Time
	var unpricedFlag2 bool
	if err := pool.QueryRow(ctx, `
		SELECT cost_minor, unpriced, priced_at FROM observability.cost
		WHERE event_id = 'evt-cost-2'`).
		Scan(&unpricedCost, &unpricedFlag2, &unpricedAt); err != nil {
		t.Fatalf("读回未定价行失败：%v", err)
	}
	if !unpricedFlag2 {
		t.Error("未命中价格表的调用必须显式标 unpriced")
	}
	if unpricedCost != 0 {
		t.Errorf("unpriced 行 cost_minor = %d，期望 0（不是半截值）", unpricedCost)
	}
	if unpricedAt != nil {
		t.Error("unpriced 行的 priced_at 应为 NULL，与「已定价」区分开")
	}

	// 重放：ON CONFLICT DO NOTHING，不报错也不写重
	if err := sink.Append(ctx, batch); err != nil {
		t.Fatalf("重放失败（成本写入应幂等）：%v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, countSQL).Scan(&after); err != nil {
		t.Fatalf("重放后统计失败：%v", err)
	}
	if after != 2 {
		t.Errorf("重放后成本行数 = %d，期望 2", after)
	}
}

// TestObsSinkCostForeignKeyRequiresEvent 钉住外键约束本身还在。
//
// 若哪天有人把 cost 的 REFERENCES 去掉，上面那条测试仍会绿，但「成本行可以
// 指向不存在的事件」就成了一个静默的数据完整性缺口。
func TestObsSinkCostForeignKeyRequiresEvent(t *testing.T) {
	pool := obsMigratedPool(t)
	ctx := context.Background()

	// 跳过 Sink，直接插一条指向不存在事件的成本行
	_, err := pool.Exec(ctx, `
		INSERT INTO observability.cost (event_id, provider, model, currency, unpriced)
		VALUES ('nonexistent-event', 'deepseek', 'deepseek-flash', 'CNY', true)`)
	if err == nil {
		t.Fatal("cost.event_id 指向不存在的事件时应当被外键拒绝")
	}
}
