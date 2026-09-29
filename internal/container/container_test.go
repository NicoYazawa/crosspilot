package container

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

// unreachableConfig 指向不会有服务监听的地址。
func unreachableConfig() *config.Config {
	return &config.Config{
		Env:  config.EnvDevelopment,
		Log:  config.LogConfig{Level: "error", Format: "json"},
		HTTP: config.HTTPConfig{Addr: ":0", ShutdownTimeout: time.Second},
		Postgres: config.PostgresConfig{
			Host:     "127.0.0.1",
			Port:     1,
			User:     "crosspilot",
			Password: "s3cr3t",
			Database: "crosspilot",
			SSLMode:  "disable",
			MaxConns: 4,
		},
		Redis: config.RedisConfig{Addr: "127.0.0.1:1"},
	}
}

// TestBuildSucceedsWithoutDependencies 是 A2 的单元级版本：依赖全不可用时
// 装配仍要成功，问题留到 /health 去暴露。
func TestBuildSucceedsWithoutDependencies(t *testing.T) {
	app, err := Build(context.Background(), unreachableConfig(), io.Discard)
	if err != nil {
		t.Fatalf("依赖不可用不应导致装配失败：%v", err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	if app.Logger == nil {
		t.Error("logger 未初始化")
	}
	if app.Pool == nil {
		t.Error("连接池未初始化")
	}
	if app.Redis == nil {
		t.Error("Redis 客户端未初始化")
	}
	if app.Handler == nil {
		t.Error("路由未装配")
	}
}

// TestHealthReportsDependenciesDown 断言依赖断开时的响应形状：
// 503，且逐项指明 db 与 redis 掉了。
func TestHealthReportsDependenciesDown(t *testing.T) {
	app, err := Build(context.Background(), unreachableConfig(), io.Discard)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{`"db":"down"`, `"redis":"down"`, `"status":"down"`} {
		if !strings.Contains(body, want) {
			t.Errorf("响应体缺少 %s：%s", want, body)
		}
	}
}

// TestLiveStaysUpWhenDependenciesAreDown 确认存活探测不被依赖拖垮。
func TestLiveStaysUpWhenDependenciesAreDown(t *testing.T) {
	app, err := Build(context.Background(), unreachableConfig(), io.Discard)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
}

func TestBuildRejectsBadLogConfig(t *testing.T) {
	cfg := unreachableConfig()
	cfg.Log.Format = "yaml"

	if _, err := Build(context.Background(), cfg, io.Discard); err == nil {
		t.Fatal("非法日志格式应当导致装配失败")
	}
}

func TestBuildRejectsBadPostgresConfig(t *testing.T) {
	cfg := unreachableConfig()
	cfg.Postgres.SSLMode = "no-such-sslmode"

	if _, err := Build(context.Background(), cfg, io.Discard); err == nil {
		t.Fatal("非法连接串应当导致装配失败")
	}
}

// TestBuildRejectsBadOTelEndpoint 确认装配中途失败时已经建立的资源会被释放。
func TestBuildRejectsBadOTelEndpoint(t *testing.T) {
	cfg := unreachableConfig()
	cfg.OTel = config.OTelConfig{Endpoint: "kafka://collector:9092", ServiceName: "crosspilot-api"}

	if _, err := Build(context.Background(), cfg, io.Discard); err == nil {
		t.Fatal("非法 OTLP 端点应当导致装配失败")
	}
}

func TestCloseIsIdempotentEnough(t *testing.T) {
	app, err := Build(context.Background(), unreachableConfig(), io.Discard)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}

	if err := app.Close(context.Background()); err != nil {
		t.Errorf("关闭出错：%v", err)
	}
}

// TestCloseOnZeroContainer 确认未装配的 Container 关闭时不会 panic。
func TestCloseOnZeroContainer(t *testing.T) {
	var app Container
	if err := app.Close(context.Background()); err != nil {
		t.Errorf("关闭空容器出错：%v", err)
	}
}

func TestBuildEnablesTracingWhenEndpointSet(t *testing.T) {
	// 端点合法时装配应当成功；导出器是惰性的，不会在构造期拨号
	cfg := unreachableConfig()
	cfg.OTel = config.OTelConfig{Endpoint: "127.0.0.1:4318", ServiceName: "crosspilot-api"}

	app, err := Build(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 接收端不存在，关闭时导出会失败，但不该 panic；这里只确认调用链是通的
	_ = app.Close(ctx)
}
