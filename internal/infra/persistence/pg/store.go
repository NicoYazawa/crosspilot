// Package pg 是交易账本与会话快照的 Postgres 实现。
//
// 这里处理的是源实现里最容易照搬错的一部分。源实现跑在 SQLite 上，靠
// `BEGIN IMMEDIATE` 取得数据库级写锁，因此「先检查库存、再写入」这段代码
// 天生是串行的；Postgres 用 MVCC，没有这个原语，直接照搬会留下窗口：
// 两个事务可能都读到「库存还有 1 件」，然后都扣一次。
//
// 因此本包不假设任何隐式的串行化，而是显式地做三件事：
//
//   - 每次 prepare/resolve 都先取一个按操作编号命名的咨询锁，
//     让「同一个操作编号」的检查与写入在数据库层串行；
//   - 库存扣减写成带条件的单条 UPDATE，并以 rowcount 判定成败，
//     让「库存够不够」由数据库在持锁状态下回答，而不是由应用层读一次再判断；
//   - 幂等读取与唯一约束并存：唯一约束保证只可能写进一行，
//     读到唯一约束冲突就回到事务开头重读，把失败路径变成重放路径。
package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// maxConflictRetries 是冲突重试上限。
//
// 超过上限就放弃并告警：无限重试会把一次死锁变成一次雪崩，
// 而调用方需要的是一个明确的失败信号，不是越来越长的等待。
const maxConflictRetries = 3

// retryBaseDelay 是重试退避的基数，第 n 次重试等待 n × 该值。
const retryBaseDelay = 5 * time.Millisecond

// 需要重试的 Postgres 错误：序列化失败与死锁都是「这一轮运气不好」，
// 重来一次通常就过；其余错误（约束冲突、类型错误）重来多少次都一样。
const (
	sqlStateSerializationFailure = "40001"
	sqlStateDeadlockDetected     = "40P01"
	sqlStateUniqueViolation      = "23505"
	sqlStateCheckViolation       = "23514"
	sqlStateForeignKeyViolation  = "23503"
)

// Config 是构造存储所需的依赖。
type Config struct {
	Pool   *pgxpool.Pool
	Clock  Clock
	Logger *slog.Logger
	// Faults 是故障注入点，仅测试使用；为 nil 时全部为空操作。
	Faults FaultInjector
	// FaultOperations 限定故障注入在哪些操作上生效；为 nil 表示不限定。
	//
	// 没有这道开关，注入点会连「准备测试数据」的那些事务一起打断，
	// 测试就没法把故障放在它真正想验证的那一步。
	FaultOperations func(operation string) bool
	// ObserveTransaction 在每次事务开始时被调用，仅测试使用。
	//
	// 故障注入点位于提交之前，因此它只能观察到「走到了提交」的那些尝试；
	// 事务体在中途返回业务错误时，提交点根本不会被执行。
	// 「重试了几次」这个问题需要独立于故障点的观察点，否则
	// 「确定性失败不重试」这类断言就数不到那唯一的一次尝试。
	ObserveTransaction func(operation string)
}

// Clock 提供当前时间。
type Clock interface {
	Now() time.Time
}

// FaultPoint 是可注入故障的位置。
//
// 逐个命名而不是一个通用钩子，是为了让测试能精确说出「在扣完库存、
// 还没写订单的时候失败」，而不是「在某个大概的位置失败」。
type FaultPoint string

const (
	// FaultAfterInventoryDebit 位于库存已扣、订单尚未写入之间。
	FaultAfterInventoryDebit FaultPoint = "after_inventory_debit"
	// FaultAfterOrderInsert 位于订单已写、决议记录尚未写入之间。
	FaultAfterOrderInsert FaultPoint = "after_order_insert"
	// FaultBeforeCommit 位于所有写入之后、提交之前。
	FaultBeforeCommit FaultPoint = "before_commit"
	// FaultAfterInventoryRestore 位于取消回补库存之后、订单状态更新之前。
	FaultAfterInventoryRestore FaultPoint = "after_inventory_restore"
)

// FaultInjector 在指定位置被调用。返回非 nil 错误即中止本次事务。
type FaultInjector func(point FaultPoint) error

// Store 是交易账本与会话快照的 Postgres 实现。
type Store struct {
	pool        *pgxpool.Pool
	clock       Clock
	logger      *slog.Logger
	faults      FaultInjector
	faultFilter func(operation string) bool
	observe     func(operation string)
}

// New 构造存储。
func New(cfg Config) (*Store, error) {
	if cfg.Pool == nil {
		return nil, errors.New("pg: 连接池未初始化")
	}
	if cfg.Clock == nil {
		return nil, errors.New("pg: 缺少时钟")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Store{
		pool:        cfg.Pool,
		clock:       cfg.Clock,
		logger:      logger,
		faults:      cfg.Faults,
		faultFilter: cfg.FaultOperations,
		observe:     cfg.ObserveTransaction,
	}, nil
}

// Now 返回存储当前使用的时间。
func (s *Store) Now() time.Time { return s.clock.Now() }

// fault 触发故障注入点。
func (s *Store) fault(operation string, point FaultPoint) error {
	if s.faults == nil {
		return nil
	}
	if s.faultFilter != nil && !s.faultFilter(operation) {
		return nil
	}
	return s.faults(point)
}

// isRetryable 报告错误是否值得重试。
//
// 两类情况会走到这里：数据库报告的序列化失败/死锁，以及被显式标记的唯一约束
// 冲突。后者不是「运气不好」而是「有人先写了一步」——重放整轮事务会读到那一行
// 并走幂等重放路径，因此同样值得重试。
func isRetryable(err error) bool {
	var marked *retryableUniqueError
	if errors.As(err, &marked) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case sqlStateSerializationFailure, sqlStateDeadlockDetected:
		return true
	default:
		return false
	}
}

// retryableUniqueError 把唯一约束冲突标记成可重试。
type retryableUniqueError struct{ err error }

// Error 实现 error。
func (e *retryableUniqueError) Error() string { return e.err.Error() }

// Unwrap 暴露底层错误。
func (e *retryableUniqueError) Unwrap() error { return e.err }

// isUniqueViolation 报告错误是否为唯一约束冲突。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == sqlStateUniqueViolation
}

// retry 执行一个事务体，在序列化失败与死锁时重试。
//
// body 会被完整重放，因此它必须是幂等的——这一点由调用方保证：
// 事务回滚后数据库里没有任何残留，重放从头开始。
func (s *Store) retry(ctx context.Context, operation string, body func(context.Context) error) error {
	var lastErr error
	for attempt := 0; attempt <= maxConflictRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(attempt) * retryBaseDelay
			s.logger.WarnContext(ctx, "事务冲突，重试",
				slog.String("operation", operation),
				slog.Int("attempt", attempt),
				slog.Duration("delay", delay),
				slog.String("error", lastErr.Error()))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		lastErr = body(ctx)
		if lastErr == nil {
			return nil
		}
		if !isRetryable(lastErr) {
			return lastErr
		}
	}

	s.logger.ErrorContext(ctx, "事务冲突重试次数耗尽",
		slog.String("operation", operation),
		slog.Int("attempts", maxConflictRetries+1),
		slog.String("error", lastErr.Error()))
	return fmt.Errorf("pg: %s 在 %d 次重试后仍未成功: %w", operation, maxConflictRetries+1, lastErr)
}

// inTx 在一个事务里执行 body，出错即回滚。
//
// operation 只用于故障注入的限定：同一个注入点在不同操作上语义不同
// （例如 before_commit 在 prepare 与 resolve 里对应完全不同的写入），
// 测试必须能指名道姓地说「在决议的提交前失败」。
//
// 回滚用独立的 context：请求 context 可能已经取消，而回滚恰恰是取消之后
// 最需要执行的操作。用已取消的 context 回滚等于不回滚。
func (s *Store) inTx(ctx context.Context, operation string, body func(pgx.Tx) error) error {
	if s.observe != nil {
		s.observe(operation)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pg: 开启事务失败: %w", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(rollbackCtx); err != nil {
			s.logger.ErrorContext(ctx, "事务回滚失败", slog.String("error", err.Error()))
		}
	}()

	if err := body(tx); err != nil {
		return err
	}

	if err := s.fault(operation, FaultBeforeCommit); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: 提交事务失败: %w", err)
	}
	committed = true
	return nil
}

// lockOperation 取得按操作编号命名的咨询锁。
//
// 事务级咨询锁在提交或回滚时自动释放，不需要显式解锁，
// 因此「忘了解锁」这类死锁不会出现。
//
// 锁的键是操作编号的哈希：同一操作编号的并发请求在这里排队，
// 「查重 → 校验 → 写入」因此不会被插队。
func lockOperation(ctx context.Context, tx pgx.Tx, operationID string) error {
	const query = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`
	if _, err := tx.Exec(ctx, query, "trade-operation:"+operationID); err != nil {
		return fmt.Errorf("pg: 获取操作锁失败: %w", err)
	}
	return nil
}

// lockInventoryInitialization 取得库存初始化的全局咨询锁。
//
// 首启时可能有多个实例同时灌种子数据，没有这把锁就会同时在读快照、
// 各自算出「这份 SKU 还没建」，然后抢着插入。
func lockInventoryInitialization(ctx context.Context, tx pgx.Tx) error {
	const query = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`
	if _, err := tx.Exec(ctx, query, "trade-inventory-init"); err != nil {
		return fmt.Errorf("pg: 获取库存初始化锁失败: %w", err)
	}
	return nil
}
