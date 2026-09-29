// Command migrate 应用或回滚数据库迁移。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/NicoYazawa/crosspilot/internal/config"
	"github.com/NicoYazawa/crosspilot/internal/infra/postgres"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "crosspilot-migrate: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("用法: migrate <up|down>")
	}
	direction := postgres.Direction(args[0])

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return err
	}
	defer pool.Close()

	// 连接池是惰性建的，先探一次，把「连不上」和「脚本有问题」区分开报
	if err := postgres.Check(ctx, pool); err != nil {
		return err
	}

	steps, err := postgres.Migrate(ctx, pool, direction)
	if err != nil {
		return err
	}

	fmt.Printf("迁移方向 %s，实际执行 %d 步\n", direction, steps)
	return nil
}
