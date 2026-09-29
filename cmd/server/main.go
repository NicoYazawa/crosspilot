// Command server 是 CrossPilot 的 HTTP 服务入口。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/config"
	"github.com/NicoYazawa/crosspilot/internal/container"
)

// closeTimeout 是退出时释放资源的时间上限。
const closeTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "crosspilot: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app, err := container.Build(ctx, cfg, os.Stderr)
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:         cfg.HTTP.Addr,
		Handler:      app.Handler,
		ReadTimeout:  cfg.HTTP.ReadTimeout,
		WriteTimeout: cfg.HTTP.WriteTimeout,
		IdleTimeout:  cfg.HTTP.IdleTimeout,
		ErrorLog:     slog.NewLogLogger(app.Logger.Handler(), slog.LevelWarn),
	}

	// 监听在独立协程里跑，主协程只负责等信号或等它出错
	serverErr := make(chan error, 1)
	go func() {
		app.Logger.Info("服务启动",
			slog.String("addr", cfg.HTTP.Addr),
			slog.String("env", cfg.Env))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		// 监听都没起来，没有流量需要排空，直接释放资源
		_ = app.Close(context.Background())
		return err
	case <-ctx.Done():
		app.Logger.Info("收到退出信号，开始优雅关闭")
	}

	// 这里用全新的 ctx：触发退出的那个 ctx 已经作废，拿它做超时会立刻到期
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		app.Logger.Error("优雅关闭超时，强制断开剩余连接", slog.Any("error", err))
		_ = server.Close()
	}

	closeCtx, cancelClose := context.WithTimeout(context.Background(), closeTimeout)
	defer cancelClose()
	if err := app.Close(closeCtx); err != nil {
		app.Logger.Error("释放资源出错", slog.Any("error", err))
	}

	app.Logger.Info("已退出")
	return nil
}
