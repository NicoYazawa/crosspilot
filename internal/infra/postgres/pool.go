// Package postgres 提供 Postgres 连接池、就绪探测与迁移执行器。
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

// pingTimeout 是单次健康探测的超时。取得较短，避免数据库失联时拖住 /health。
const pingTimeout = 2 * time.Second

// NewPool 构造连接池。
//
// 构造过程不建立实际连接：MinConns 置零后 pgxpool 完全惰性，因此数据库暂时
// 不可用也不会让启动失败。这是刻意的——服务必须能起来并如实报告 db: down，
// 而不是在数据库抖动时崩溃重启、放大故障。
func NewPool(ctx context.Context, cfg config.PostgresConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		return nil, fmt.Errorf("postgres: 解析连接串失败: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = 0
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	// 建连超时短于健康探测超时，让不可达的目标快速失败而不是挂住探测
	poolCfg.ConnConfig.ConnectTimeout = pingTimeout

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: 创建连接池失败: %w", err)
	}
	return pool, nil
}

// Check 探测数据库是否可用，供健康检查调用。
func Check(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return fmt.Errorf("postgres: 连接池未初始化")
	}

	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: 探测失败: %w", err)
	}
	return nil
}
