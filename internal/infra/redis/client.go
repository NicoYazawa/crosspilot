// Package redis 提供 Redis 客户端与就绪探测。
package redis

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

// pingTimeout 是单次连接与健康探测的超时，取得较短以免拖住 /health。
const pingTimeout = 2 * time.Second

// NewClient 构造 Redis 客户端。
//
// 与 Postgres 连接池同理，构造过程不建立实际连接，Redis 暂时不可用不会
// 导致启动失败；就绪性由 Check 单独探测。
//
// logger 可为 nil；给出时客户端自身的日志并入结构化日志，否则它会直接写
// stderr，绕过日志配置也带不上请求关联字段。
func NewClient(cfg config.RedisConfig, logger *slog.Logger) *goredis.Client {
	options := &goredis.Options{
		Addr:         cfg.Addr,
		Password:     cfg.Password.Reveal(),
		DB:           cfg.DB,
		DialTimeout:  pingTimeout,
		ReadTimeout:  pingTimeout,
		WriteTimeout: pingTimeout,
		// 客户端内部不重试：健康探测要的是「此刻通不通」，
		// 连不上就立刻说连不上，重试只会把 /health 拖慢若干倍。
		// MaxRetries 的 -1 是「关闭」的入参约定，客户端内部会归一为 0；
		// DialerRetries 则相反，小于等于 0 会被当成默认的 5 次
		MaxRetries:    -1,
		DialerRetries: 1,
	}
	if logger != nil {
		// 客户端只提供全局日志入口，没有按实例配置的位置；
		// 全进程只有一个 Redis 客户端，因此这里不引入额外的协调
		goredis.SetLogger(slogLogger{logger: logger})
	}
	return goredis.NewClient(options)
}

// slogLogger 把客户端内部日志转接到 slog。
type slogLogger struct {
	logger *slog.Logger
}

// Printf 实现客户端的日志接口。它的输出偏诊断性质，按 debug 级别记录。
func (l slogLogger) Printf(ctx context.Context, format string, args ...any) {
	l.logger.DebugContext(ctx, "redis: "+fmt.Sprintf(format, args...))
}

// Check 探测 Redis 是否可用，供健康检查调用。
func Check(ctx context.Context, client *goredis.Client) error {
	if client == nil {
		return fmt.Errorf("redis: 客户端未初始化")
	}

	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis: 探测失败: %w", err)
	}
	return nil
}
