package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	domainobs "github.com/NicoYazawa/crosspilot/internal/domain/observability"
)

// ExperimentStore 是 application/observability.ExperimentStore 的 Postgres 实现。
//
// A/B 元信息唯一的事实源是 observability.run 的 experiment_key / experiment_arm，
// 本存储只读不写——分组由上游写路径决定。
type ExperimentStore struct {
	pool *pgxpool.Pool
}

// NewExperimentStore 构造实验读存储。
func NewExperimentStore(pool *pgxpool.Pool) *ExperimentStore {
	return &ExperimentStore{pool: pool}
}

// ArmFor 返回 run 所属的实验臂。
//
// 无该 run 行、或该行的 experiment_arm 为 NULL/空串 → ("", false, nil)：
// 「没进实验」是合法状态，不是错误，因此 ErrNoRows 也必须按无臂处理。
func (s *ExperimentStore) ArmFor(ctx context.Context, runID string) (string, bool, error) {
	const query = `SELECT experiment_arm FROM observability.run WHERE run_id = $1`

	// 用指针承接可空列：NULL 与空串要能区分于「查无此行」。
	var arm *string
	err := s.pool.QueryRow(ctx, query, runID).Scan(&arm)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("pg: 查询实验臂失败: %w", err)
	}
	if arm == nil || *arm == "" {
		return "", false, nil
	}
	return *arm, true, nil
}

// ArmsSummary 返回某个实验各臂的聚合统计，按 arm 升序。
//
// 分组只取 experiment_arm IS NOT NULL 的 run。各臂的四项指标里，LatencyP95 只看
// 有 finished_at 的 run；成本相关指标来自 event→cost 的 join。两个 CTE 分开算再
// LEFT JOIN，是为了避免 run×cost 的笛卡尔积把 Calls 与 P95 一起放大——把它们写在
// 同一个 GROUP BY 里，加一个成本行就会多算一次调用。
//
// 两种「空」必须分开：key 从未出现过任何 run → ErrExperimentNotFound
// （HTTP 层据此回 404，提示「实验不存在」）；key 下有 run 但都还没分臂 →
// 空切片 + nil 错误（「实验刚建好还没数据」）。把后者也说成「不存在」，
// 运维就永远看不到「实验已创建但没跑起来」这个真正的问题。
func (s *ExperimentStore) ArmsSummary(ctx context.Context, key string) ([]domainobs.ArmSummary, error) {
	// currency 用 MAX 而不是 SUM/AVG：一个臂内正常只应有一种币种，
	// 出现多种就是数据错误。这里取字典序最大者作为确定性的代表值，
	// 既保证同一份数据每次输出一致，也不去跨币种做无意义的求和。
	const query = `
		WITH arm_runs AS (
			SELECT experiment_arm AS arm, run_id, started_at, finished_at
			FROM observability.run
			WHERE experiment_key = $1 AND experiment_arm IS NOT NULL
		),
		run_metrics AS (
			SELECT arm,
			       COUNT(*) AS calls,
			       COALESCE(
			           percentile_disc(0.95) WITHIN GROUP (
			               ORDER BY EXTRACT(EPOCH FROM (finished_at - started_at)) * 1000
			           ) FILTER (WHERE finished_at IS NOT NULL),
			           0
			       )::bigint AS latency_p95_ms
			FROM arm_runs
			GROUP BY arm
		),
		cost_metrics AS (
			SELECT r.arm AS arm,
			       COALESCE(SUM(c.cost_minor), 0)::bigint AS cost_total_minor,
			       COALESCE(COUNT(*) FILTER (WHERE c.unpriced), 0)::bigint AS unpriced_count,
			       COALESCE(MAX(c.currency), '')::text AS currency
			FROM arm_runs r
			JOIN observability.event e ON e.run_id = r.run_id
			JOIN observability.cost c ON c.event_id = e.event_id
			GROUP BY r.arm
		)
		SELECT rm.arm,
		       rm.calls,
		       rm.latency_p95_ms,
		       COALESCE(cm.cost_total_minor, 0)::bigint,
		       COALESCE(cm.currency, '')::text,
		       COALESCE(cm.unpriced_count, 0)::bigint
		FROM run_metrics rm
		LEFT JOIN cost_metrics cm ON cm.arm = rm.arm
		ORDER BY rm.arm ASC`

	rows, err := s.pool.Query(ctx, query, key)
	if err != nil {
		return nil, fmt.Errorf("pg: 查询实验臂汇总失败: %w", err)
	}
	defer rows.Close()

	out := make([]domainobs.ArmSummary, 0)
	for rows.Next() {
		var a domainobs.ArmSummary
		if err := rows.Scan(
			&a.Arm, &a.Calls, &a.LatencyP95Ms,
			&a.CostTotalMinor, &a.Currency, &a.UnpricedCount,
		); err != nil {
			return nil, fmt.Errorf("pg: 扫描实验臂汇总失败: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 遍历实验臂汇总失败: %w", err)
	}
	if len(out) == 0 {
		registered, err := s.experimentRegistered(ctx, key)
		if err != nil {
			return nil, err
		}
		if !registered {
			return nil, domainobs.ErrExperimentNotFound
		}
	}
	return out, nil
}

// experimentRegistered 报告该 key 是否至少被一个 run 引用过。
//
// 只在聚合结果为空时才查：命中数据的常见路径不该为一次存在性判断多付一轮往返。
func (s *ExperimentStore) experimentRegistered(ctx context.Context, key string) (bool, error) {
	const q = `SELECT EXISTS (SELECT 1 FROM observability.run WHERE experiment_key = $1)`

	var exists bool
	if err := s.pool.QueryRow(ctx, q, key).Scan(&exists); err != nil {
		return false, fmt.Errorf("pg: 查询实验 key 是否注册失败: %w", err)
	}
	return exists, nil
}
