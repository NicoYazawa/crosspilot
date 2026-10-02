package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	domainobs "github.com/NicoYazawa/crosspilot/internal/domain/observability"
)

// CostStore 是 application/observability.CostStore 的 Postgres 实现。
//
// 成本行按 event_id 挂在事件上，run 到成本的路径是 event 上的 run_id，
// 因此查询必须 join event 才能按 run 归集。
type CostStore struct {
	pool *pgxpool.Pool
}

// NewCostStore 构造成本读存储。
func NewCostStore(pool *pgxpool.Pool) *CostStore {
	return &CostStore{pool: pool}
}

// CostOfRun 返回该 run 的全部成本事件，按事件 seq 升序。
//
// F4 契约：unpriced 逐行从列读出，绝不在读取侧改写它，也绝不对 unpriced 行
// 现算成本——「未定价」必须以「未定价」的身份传到面板。
// run 无成本行时返回空非 nil 切片（[]CostEvent{}），不是 nil、也不是错误。
func (s *CostStore) CostOfRun(ctx context.Context, runID string) ([]domainobs.CostEvent, error) {
	const query = `
		SELECT c.event_id, e.run_id,
		       c.provider, c.model,
		       c.tokens_in, c.tokens_out, c.tokens_cached, c.tokens_reasoning,
		       c.cost_minor, c.currency, c.unpriced
		FROM observability.cost c
		JOIN observability.event e ON e.event_id = c.event_id
		WHERE e.run_id = $1
		ORDER BY e.seq`

	rows, err := s.pool.Query(ctx, query, runID)
	if err != nil {
		return nil, fmt.Errorf("pg: 查询成本事件失败: %w", err)
	}
	defer rows.Close()

	// 显式初始化成非 nil：调用方（HTTP 层）会把空与 nil 区别对待。
	out := make([]domainobs.CostEvent, 0)
	for rows.Next() {
		var c domainobs.CostEvent
		if err := rows.Scan(
			&c.EventID, &c.RunID,
			&c.Provider, &c.Model,
			&c.TokensIn, &c.TokensOut, &c.TokensCached, &c.TokensReasoning,
			&c.CostMinor, &c.Currency, &c.Unpriced,
		); err != nil {
			return nil, fmt.Errorf("pg: 扫描成本事件失败: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("pg: 遍历成本事件失败: %w", err)
	}
	return out, nil
}
