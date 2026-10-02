package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	domainobs "github.com/NicoYazawa/crosspilot/internal/domain/observability"
)

// obsEventBatchSize 是单次 pgx.Batch 的事件行数上限。
//
// 观测是「尽量写」的下游：一次 batch 太大只会把失败的影响面放大，
// 而 Emitter 本身按 64 攒批，这里对齐同一个量级即可。
const obsEventBatchSize = 64

// ObsSink 是 agent/observability.Sink 的 Postgres 实现，落 observability.event。
//
// 写入语义（与 F4/F6 闸门一致）：
//   - 整批一个事务：要么都进，要么都不进，不给观测留下半批脏数据
//   - 事件按 event_id 幂等（ON CONFLICT DO NOTHING），重试不会写重
//   - 不做任何阻塞式重试：写失败由 Emitter 计入 dropped，绝不拖慢 Agent
type ObsSink struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewObsSink 构造观测 Sink。logger 为 nil 时退化为丢弃日志。
func NewObsSink(pool *pgxpool.Pool, logger *slog.Logger) *ObsSink {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &ObsSink{pool: pool, logger: logger}
}

// Append 把一批观测记录写入 observability.event。
//
// 先补 run 行再写事件：event.run_id 有外键指向 run(run_id)，而观测可能先于
// 真正的 run 注册到达（例如 Run 刚起步就崩溃）。见下方占位行说明。
func (s *ObsSink) Append(ctx context.Context, batch []domainobs.SinkRecord) error {
	if len(batch) == 0 {
		return nil
	}
	if s.pool == nil {
		return errors.New("pg: 观测 sink 连接池未初始化")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: 开启观测写入事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // 已提交时回滚是空操作

	if err := s.ensureRuns(ctx, tx, batch); err != nil {
		return err
	}
	if err := s.insertEvents(ctx, tx, batch); err != nil {
		return err
	}
	// 必须在 insertEvents **之后**：cost.event_id 有外键指向 event.event_id，
	// 反过来写会撞「事件行还不存在」。同一个事务里，顺序就是全部约束。
	if err := s.insertCosts(ctx, tx, batch); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: 提交观测写入事务失败: %w", err)
	}
	return nil
}

// ensureRuns 为批内每个不同的 run_id 补一条占位 run 行，满足 event 的外键。
//
// 设计取舍：SinkRecord 不携带 buyer_id / session_id，而 run 表这两列 NOT NULL。
// 这里写占位空串并 ON CONFLICT DO NOTHING —— 观测端只负责「外键指向的行存在」，
// 不承担 run 元信息职责；真正的 buyer/session 归上游 run 注册写路径。
// 选 DO NOTHING 而非 DO UPDATE：观测是旁路写入，绝不应该在并发下反向覆盖
// 上游刚写入的真实元信息（DO UPDATE 会把占位空串刷上去，那是数据倒退）。
func (s *ObsSink) ensureRuns(ctx context.Context, tx pgx.Tx, batch []domainobs.SinkRecord) error {
	const query = `
		INSERT INTO observability.run (run_id, buyer_id, session_id)
		VALUES ($1, '', '')
		ON CONFLICT (run_id) DO NOTHING`

	seen := make(map[string]struct{}, len(batch))
	// 按下标取址：SinkRecord 约 128 字节，逐条拷贝整个结构体只是白给；
	// 这里只读字段，指针与原值语义相同。
	for i := range batch {
		rec := &batch[i]
		if rec.RunID == "" {
			return errors.New("pg: 观测记录缺少 run_id")
		}
		if _, ok := seen[rec.RunID]; ok {
			continue
		}
		seen[rec.RunID] = struct{}{}
		if _, err := tx.Exec(ctx, query, rec.RunID); err != nil {
			return fmt.Errorf("pg: 补写 observability.run 失败（run_id=%s）: %w", rec.RunID, err)
		}
	}
	return nil
}

// insertEvents 分块写入事件，单块 ≤ obsEventBatchSize。
func (s *ObsSink) insertEvents(ctx context.Context, tx pgx.Tx, batch []domainobs.SinkRecord) error {
	for start := 0; start < len(batch); start += obsEventBatchSize {
		end := start + obsEventBatchSize
		if end > len(batch) {
			end = len(batch)
		}
		if err := s.insertChunk(ctx, tx, batch[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// insertChunk 用一条 pgx.Batch 写一块事件。
func (s *ObsSink) insertChunk(ctx context.Context, tx pgx.Tx, chunk []domainobs.SinkRecord) error {
	const query = `
		INSERT INTO observability.event
			(event_id, run_id, seq, kind, agent, payload_sha256, payload_size, payload_redacted, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (event_id) DO NOTHING`

	b := &pgx.Batch{}
	// 同上：按下标取址避免逐条拷贝 SinkRecord。
	for i := range chunk {
		rec := &chunk[i]
		// payload_redacted 是 NOT NULL JSONB：空载荷落 '{}'，而非用 NULL 表达。
		payload := rec.PayloadRedacted
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		b.Queue(query,
			rec.EventID,
			rec.RunID,
			rec.Seq,
			rec.Kind,
			rec.Agent,
			rec.PayloadSHA256,
			rec.PayloadSize,
			payload,
			unixSecToUTC(rec.CreatedAt),
		)
	}

	br := tx.SendBatch(ctx, b)
	var firstErr error
	firstIdx := -1
	for i := range chunk {
		if _, err := br.Exec(); err != nil && firstErr == nil {
			firstErr = err
			firstIdx = i
		}
	}
	closeErr := br.Close()
	if firstErr == nil {
		firstErr = closeErr
	}

	if firstErr != nil {
		// 事件按 event_id 幂等；能走到这里说明可能撞上 UNIQUE(run_id, seq)，
		// 那是真实的序号冲突，必须让它冒出去而不是静默吞掉。
		if firstIdx >= 0 {
			rec := chunk[firstIdx]
			s.logger.ErrorContext(ctx, "观测事件写入失败（可能是 run_id+seq 冲突）",
				slog.String("event_id", rec.EventID),
				slog.String("run_id", rec.RunID),
				slog.Int64("seq", rec.Seq),
				slog.String("error", firstErr.Error()))
		}
		return fmt.Errorf("pg: 写入观测事件失败: %w", firstErr)
	}
	return nil
}

// insertCosts 写入批内带成本的事件行，单块 ≤ obsEventBatchSize。
//
// 只有 Cost 非 nil 的记录会写：非模型调用（工具、A2UI、run 起止）本就不产生
// 费用，给它们写一行全 0 的成本会让「这条事件花了 0 元」和「这条事件不涉及
// 计费」在表里长得一样，成本看板的 total_calls 也会虚高。
func (s *ObsSink) insertCosts(ctx context.Context, tx pgx.Tx, batch []domainobs.SinkRecord) error {
	for start := 0; start < len(batch); start += obsEventBatchSize {
		end := start + obsEventBatchSize
		if end > len(batch) {
			end = len(batch)
		}
		if err := s.insertCostChunk(ctx, tx, batch[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// insertCostChunk 用一条 pgx.Batch 写一块成本行。
func (s *ObsSink) insertCostChunk(ctx context.Context, tx pgx.Tx, chunk []domainobs.SinkRecord) error {
	const query = `
		INSERT INTO observability.cost
			(event_id, provider, model, tokens_in, tokens_out, tokens_cached,
			 tokens_reasoning, cost_minor, currency, unpriced, priced_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (event_id) DO NOTHING`

	b := &pgx.Batch{}
	// idx 记录批内第几条是成本行：失败时要能回报是哪一个 event_id 出的问题，
	// 否则一条 batch 里混着事件与成本两类语句，日志只能指向 chunk 下标。
	idx := make([]int, 0, len(chunk))
	for i := range chunk {
		rec := &chunk[i]
		c := rec.Cost
		if c == nil {
			continue
		}
		// priced_at 只在真的定了价的时候写：unpriced 行的 priced_at 留 NULL，
		// 「试过但没价目」与「定了价」在 SQL 里才分得开（看板按它算覆盖率）。
		var pricedAt *time.Time
		if !c.Unpriced {
			now := time.Now().UTC()
			pricedAt = &now
		}
		currency := c.Currency
		if currency == "" {
			currency = "CNY"
		}
		// 外键用 rec.EventID 而不是 c.EventID：本行的语义是「这条事件的成本」，
		// 两者本应相等，但真出现分歧时，外键必须指向确实刚写进去的那一行。
		b.Queue(query,
			rec.EventID,
			c.Provider,
			c.Model,
			c.TokensIn,
			c.TokensOut,
			c.TokensCached,
			c.TokensReasoning,
			c.CostMinor,
			currency,
			c.Unpriced,
			pricedAt,
		)
		idx = append(idx, i)
	}
	if len(idx) == 0 {
		return nil
	}

	br := tx.SendBatch(ctx, b)
	var firstErr error
	firstIdx := -1
	for n := range idx {
		if _, err := br.Exec(); err != nil && firstErr == nil {
			firstErr = err
			firstIdx = n
		}
	}
	closeErr := br.Close()
	if firstErr == nil {
		firstErr = closeErr
	}
	if firstErr != nil {
		if firstIdx >= 0 {
			rec := chunk[idx[firstIdx]]
			s.logger.ErrorContext(ctx, "观测成本写入失败",
				slog.String("event_id", rec.EventID),
				slog.String("run_id", rec.RunID),
				slog.String("provider", rec.Cost.Provider),
				slog.String("model", rec.Cost.Model),
				slog.String("error", firstErr.Error()))
		}
		return fmt.Errorf("pg: 写入观测成本失败: %w", firstErr)
	}
	return nil
}

// Close 是空操作，返回 nil：连接池由容器拥有，Sink 不负责其生命周期。
func (s *ObsSink) Close() error { return nil }

// unixSecToUTC 把 SinkRecord 的 Unix 秒转成 TIMESTAMPTZ 需要的 UTC 时刻。
func unixSecToUTC(sec int64) time.Time {
	return time.Unix(sec, 0).UTC()
}
