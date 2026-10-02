package pg_test

import (
	"context"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
)

// seedCostRun 灌入一个 run 及其两条成本行：一条已定价、一条未定价。
//
// 直接走 SQL 而不是 K 走 Sink：本用例要验证的是读取端的 unpriced 保真，
// 用最小的事实源比经由写路径更能钉住「读出来的东西和库里一致」。
func seedCostRun(t *testing.T) *pg.CostStore {
	t.Helper()
	pool := obsMigratedPool(t)
	ctx := context.Background()

	stmts := []string{
		`INSERT INTO observability.run (run_id, buyer_id, session_id) VALUES ('run-cost-1', 'b', 's')`,
		`INSERT INTO observability.event (event_id, run_id, seq, kind, payload_sha256, payload_size)
		 VALUES ('ce-1', 'run-cost-1', 1, 'llm.call', 'h1', 10),
		        ('ce-2', 'run-cost-1', 2, 'llm.call', 'h2', 20)`,
		`INSERT INTO observability.cost
		     (event_id, provider, model, tokens_in, tokens_out, tokens_cached, tokens_reasoning, cost_minor, currency, unpriced)
		 VALUES ('ce-1', 'qwen', 'qwen-max', 100, 50, 10, 5, 1500, 'CNY', false),
		        ('ce-2', 'deepseek', 'deepseek-chat', 20, 10, 0, 0, 0, 'CNY', true)`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("灌种子失败：%v\nSQL: %s", err, stmt)
		}
	}
	return pg.NewCostStore(pool)
}

// TestPgCostStoreEmptyRun 覆盖：无成本行的 run 返回空非 nil 切片与 nil 错误。
func TestPgCostStoreEmptyRun(t *testing.T) {
	store := seedCostRun(t)

	got, err := store.CostOfRun(context.Background(), "run-without-cost")
	if err != nil {
		t.Fatalf("无成本行不应报错，实际：%v", err)
	}
	if got == nil {
		t.Fatal("无成本行应返回空非 nil 切片，实际 nil")
	}
	if len(got) != 0 {
		t.Fatalf("无成本行切片长度 = %d，期望 0", len(got))
	}
}

// TestPgCostStoreUnpricedFaithful 覆盖：定价行与未定价行都返回，且 unpriced 逐行保真。
func TestPgCostStoreUnpricedFaithful(t *testing.T) {
	store := seedCostRun(t)

	got, err := store.CostOfRun(context.Background(), "run-cost-1")
	if err != nil {
		t.Fatalf("查询成本失败：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("成本行数 = %d，期望 2", len(got))
	}

	// 按 event.seq 升序：ce-1 在前（已定价），ce-2 在后（未定价）。
	priced, unpriced := got[0], got[1]

	if priced.EventID != "ce-1" || priced.RunID != "run-cost-1" {
		t.Errorf("首行标识 = (%s, %s)，期望 (ce-1, run-cost-1)", priced.EventID, priced.RunID)
	}
	if priced.Provider != "qwen" || priced.Model != "qwen-max" {
		t.Errorf("首行 provider/model = (%s, %s)，期望 (qwen, qwen-max)", priced.Provider, priced.Model)
	}
	if priced.TokensIn != 100 || priced.TokensOut != 50 || priced.TokensCached != 10 || priced.TokensReasoning != 5 {
		t.Errorf("首行 token = (%d,%d,%d,%d)，期望 (100,50,10,5)",
			priced.TokensIn, priced.TokensOut, priced.TokensCached, priced.TokensReasoning)
	}
	if priced.CostMinor != 1500 || priced.Currency != "CNY" {
		t.Errorf("首行成本 = (%d, %s)，期望 (1500, CNY)", priced.CostMinor, priced.Currency)
	}
	if priced.Unpriced {
		t.Error("首行 unpriced = true，期望 false")
	}

	if unpriced.EventID != "ce-2" {
		t.Errorf("次行 event_id = %s，期望 ce-2", unpriced.EventID)
	}
	if !unpriced.Unpriced {
		t.Error("次行 unpriced = false，期望 true（F4：未定价必须原样保留）")
	}
	if unpriced.CostMinor != 0 {
		t.Errorf("次行 cost_minor = %d，期望 0（不得对未定价行现算成本）", unpriced.CostMinor)
	}
}
