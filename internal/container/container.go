// Package container 是唯一的装配根：把配置、基础设施与接口层接到一起。
//
// 依赖只在这里创建并向下注入，其他包一律通过构造函数接收所需依赖，不自行
// 读取全局状态、不自行连接外部服务。依赖方向因此是单向的，测试里也能按需
// 替换任意一层。
package container

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/NicoYazawa/crosspilot/internal/config"
	"github.com/NicoYazawa/crosspilot/internal/infra/logging"
	"github.com/NicoYazawa/crosspilot/internal/infra/postgres"
	infraredis "github.com/NicoYazawa/crosspilot/internal/infra/redis"
	"github.com/NicoYazawa/crosspilot/internal/observability"
	presentation "github.com/NicoYazawa/crosspilot/internal/presentation/http"
)

// 探测项在 /health 响应里的键名。
const (
	checkDatabase = "db"
	checkRedis    = "redis"
)

// Container 持有进程运行期间的长生命周期依赖。
type Container struct {
	Config  *config.Config
	Logger  *slog.Logger
	Pool    *pgxpool.Pool
	Redis   *goredis.Client
	Handler http.Handler

	shutdownOTel func(context.Context) error
}

// Build 按配置装配整个进程。
//
// 装配过程不建立真实连接：数据库或缓存暂时不可用不会让启动失败，就绪性由
// /health 单独探测。这样依赖恢复之前服务就能先起来，编排系统也能看到明确的
// 「已启动但未就绪」，而不是反复重启一个起不来的进程。
func Build(ctx context.Context, cfg *config.Config, stderr io.Writer) (*Container, error) {
	logger, err := logging.New(cfg.Log, stderr)
	if err != nil {
		return nil, err
	}

	shutdownOTel, err := observability.Setup(ctx, cfg.OTel)
	if err != nil {
		return nil, err
	}

	pool, err := postgres.NewPool(ctx, cfg.Postgres)
	if err != nil {
		// 装配失败时已经建立的资源要还回去，否则导出器的后台协程会留在原地
		_ = shutdownOTel(ctx)
		return nil, err
	}

	redisClient := infraredis.NewClient(cfg.Redis, logger)

	return &Container{
		Config:       cfg,
		Logger:       logger,
		Pool:         pool,
		Redis:        redisClient,
		Handler:      buildRouter(cfg, logger, pool, redisClient),
		shutdownOTel: shutdownOTel,
	}, nil
}

func buildRouter(
	cfg *config.Config,
	logger *slog.Logger,
	pool *pgxpool.Pool,
	redisClient *goredis.Client,
) http.Handler {
	return presentation.NewRouter(presentation.Deps{
		Config: cfg,
		Logger: logger,
		Checks: []presentation.Check{
			{
				Name:  checkDatabase,
				Probe: func(ctx context.Context) error { return postgres.Check(ctx, pool) },
			},
			{
				Name:  checkRedis,
				Probe: func(ctx context.Context) error { return infraredis.Check(ctx, redisClient) },
			},
		},
		// Observability handler：P5 阶段暂不在容器层接入（依赖 CostStore /
		// ExperimentStore 的 Postgres 实现，留待 P5 收尾阶段接入；当前
		// handler_test.go 已覆盖单元测试回路）。
	})
}

// Close 释放全部资源，并汇总关闭过程中的错误。
//
// 即使前一项关闭失败也继续关剩下的：漏关资源比多一条错误信息严重得多。
func (c *Container) Close(ctx context.Context) error {
	var errs []error

	if c.Redis != nil {
		if err := c.Redis.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if c.Pool != nil {
		c.Pool.Close()
	}
	if c.shutdownOTel != nil {
		if err := c.shutdownOTel(ctx); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}
