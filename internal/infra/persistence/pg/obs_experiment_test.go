package pg_test

import (
	"context"
	"errors"
	"testing"

	domainobs "github.com/NicoYazawa/crosspilot/internal/domain/observability"
	"github.com/NicoYazawa/crosspilot/internal/infra/persistence/pg"
)

// seedExperiment 灌入一个两臂实验（a / b）与一个无臂 run。
//
// 臂 a 两次调用（P95=200ms，1 条未定价），臂 b 一次调用（P95=300ms，全已定价）：
// 数值刻意取不同，好让聚合算错时立刻看出来。
func seedExperiment(t *testing.T) *pg.ExperimentStore {
	t.Helper()
	pool := obsMigratedPool(t)
	ctx := context.Background()

	stmts := []string{
		// 两臂实验：a1/a2 属 a，b1 属 b。
		`INSERT INTO observability.run (run_id, buyer_id, session_id, experiment_key, experiment_arm, started_at, finished_at)
		 VALUES ('run-a1', 'b', 's', 'exp-1', 'a', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00.1+00'),
		        ('run-a2', 'b', 's', 'exp-1', 'a', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00.2+00'),
		        ('run-b1', 'b', 's', 'exp-1', 'b', '2026-01-01 00:00:00+00', '2026-01-01 00:00:00.3+00')`,
		// 一个注册了 key 但没分到臂的 run：ArmFor 必须按「无臂」返回。
		`INSERT INTO observability.run (run_id, buyer_id, session_id, experiment_key)
		 VALUES ('run-noarm', 'b', 's', 'exp-null-arm')`,
		// 每个 run 一条事件，用于挂成本。
		`INSERT INTO observability.event (event_id, run_id, seq, kind, payload_sha256, payload_size)
		 VALUES ('ea1', 'run-a1', 1, 'llm.call', 'h', 1),
		        ('ea2', 'run-a2', 1, 'llm.call', 'h', 1),
		        ('eb1', 'run-b1', 1, 'llm.call', 'h', 1)`,
		// 成本：a 臂 100 分 + 1 条未定价；b 臂 250 分。
		`INSERT INTO observability.cost (event_id, provider, model, cost_minor, currency, unpriced)
		 VALUES ('ea1', 'qwen', 'qwen-max', 100, 'CNY', false),
		        ('ea2', 'qwen', 'qwen-max', 0, 'CNY', true),
		        ('eb1', 'qwen', 'qwen-max', 250, 'CNY', false)`,
	}
	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("灌种子失败：%v\nSQL: %s", err, stmt)
		}
	}
	return pg.NewExperimentStore(pool)
}

// TestPgExperimentArmFor 覆盖：无该 run、run 有 key 但 arm 为空，都按「无臂」返回。
func TestPgExperimentArmFor(t *testing.T) {
	store := seedExperiment(t)
	ctx := context.Background()

	t.Run("未注册的 run", func(t *testing.T) {
		arm, ok, err := store.ArmFor(ctx, "no-such-run")
		if err != nil {
			t.Fatalf("不应报错，实际：%v", err)
		}
		if ok || arm != "" {
			t.Fatalf("= (%q, %v)，期望 (\"\", false)", arm, ok)
		}
	})

	t.Run("run 存在但无臂", func(t *testing.T) {
		arm, ok, err := store.ArmFor(ctx, "run-noarm")
		if err != nil {
			t.Fatalf("不应报错，实际：%v", err)
		}
		if ok || arm != "" {
			t.Fatalf("= (%q, %v)，期望 (\"\", false)", arm, ok)
		}
	})

	t.Run("已分臂", func(t *testing.T) {
		arm, ok, err := store.ArmFor(ctx, "run-a1")
		if err != nil {
			t.Fatalf("不应报错，实际：%v", err)
		}
		if !ok || arm != "a" {
			t.Fatalf("= (%q, %v)，期望 (\"a\", true)", arm, ok)
		}
	})
}

// TestPgExperimentArmsSummaryUnknownKey 覆盖：未注册的 key 返回 ErrExperimentNotFound。
func TestPgExperimentArmsSummaryUnknownKey(t *testing.T) {
	store := seedExperiment(t)

	_, err := store.ArmsSummary(context.Background(), "exp-does-not-exist")
	if !errors.Is(err, domainobs.ErrExperimentNotFound) {
		t.Fatalf("err = %v，期望 ErrExperimentNotFound", err)
	}
}

// TestPgExperimentArmsSummary 覆盖：两臂聚合的 Calls / P95 / Cost / Unpriced 与排序。
func TestPgExperimentArmsSummary(t *testing.T) {
	store := seedExperiment(t)

	got, err := store.ArmsSummary(context.Background(), "exp-1")
	if err != nil {
		t.Fatalf("查询实验汇总失败：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("臂数 = %d，期望 2", len(got))
	}
	// 排序必须按 arm 升序。
	if got[0].Arm != "a" || got[1].Arm != "b" {
		t.Fatalf("臂顺序 = [%s, %s]，期望 [a, b]", got[0].Arm, got[1].Arm)
	}

	want := []domainobs.ArmSummary{
		{Arm: "a", Calls: 2, LatencyP95Ms: 200, CostTotalMinor: 100, Currency: "CNY", UnpricedCount: 1},
		{Arm: "b", Calls: 1, LatencyP95Ms: 300, CostTotalMinor: 250, Currency: "CNY", UnpricedCount: 0},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("臂 %d = %+v，期望 %+v", i, got[i], want[i])
		}
	}
}
