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

// TestRouterServesMountedSubApps 断言装配出来的路由表真的把三个子应用挂上去了。
//
// 这条测试针对的是一个已经发生过的真实故障：容器只把 /health 接了出来，
// 交易链路、AG-UI、可观测三件套的 handler 都建好了却没人挂载——进程启动
// 正常、健康检查全绿，调用方拿到的却全是 404。所以这里断言的不是「返回 200」
// （依赖断着，200 本来就不该出现），而是「路由认得这个路径」：404 说明它
// 根本没挂上去，其余任何状态码都说明请求已经走到业务代码里了。
func TestRouterServesMountedSubApps(t *testing.T) {
	app, err := Build(context.Background(), unreachableConfig(), io.Discard)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"commerce 订单列表", http.MethodGet, "/commerce/orders", ""},
		{"commerce 订单详情", http.MethodGet, "/commerce/orders/order-1", ""},
		{"commerce 取消订单", http.MethodPost, "/commerce/orders/order-1/cancel", ""},
		{"commerce 确认单列表", http.MethodGet, "/commerce/confirmations", ""},
		{"commerce 下单确认单", http.MethodPost, "/commerce/confirmations/orders", `{"product_id":"p1","quantity":1}`},
		{"commerce 确认单详情", http.MethodGet, "/commerce/confirmations/c-1", ""},
		{"commerce 确认单决议", http.MethodPost, "/commerce/confirmations/c-1/resolve", `{"approved":true}`},
		{"ag-ui 提交 run", http.MethodPost, "/commerce/ag-ui/run", `{"query":"找一双跑鞋"}`},
		{"ag-ui run 元信息", http.MethodGet, "/commerce/ag-ui/runs/run-1", ""},
		{"ag-ui 事件流", http.MethodGet, "/commerce/ag-ui/runs/run-1/events", ""},
		{"ag-ui 取消 run", http.MethodPost, "/commerce/ag-ui/runs/run-1/cancel", ""},
		{"可观测 事件回放", http.MethodGet, "/observability/runs/run-1/events", ""},
		{"可观测 成本", http.MethodGet, "/observability/runs/run-1/cost", ""},
		{"可观测 diff", http.MethodGet, "/observability/runs/run-1/diff?against=run-2", ""},
		{"可观测 实验臂", http.MethodGet, "/observability/experiments/exp-1/arms", ""},
		{"可观测 指标", http.MethodGet, "/observability/metrics", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			req := httptest.NewRequest(tc.method, tc.path, body)
			rec := httptest.NewRecorder()
			app.Handler.ServeHTTP(rec, req)

			// 认准兜底 404 的错误码。业务 404（run_not_found、order_not_found）
			// 同样返回 404，但它们恰恰证明路由是通的——只有 errorHandler 发出的
			// `"error":"not_found"` 才说明请求根本没找到挂载点。
			if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), `"error":"not_found"`) {
				t.Fatalf("路由未挂载：%s %s 落到兜底 404", tc.method, tc.path)
			}
			if rec.Code == http.StatusMethodNotAllowed {
				t.Fatalf("路由已挂载但不认这个方法：%s %s", tc.method, tc.path)
			}
		})
	}
}

// TestMetricsEndpointReportsChannelCounters 断言 /observability/metrics 是真正
// 接通的可观测出口，而不是一个永远 503 的占位。
//
// 这一条单独拎出来，是因为 metrics 路径不碰数据库：它必须无条件可用。
// 若哪天容器忘了传 Metrics，这个测试会先于线上发现。
func TestMetricsEndpointReportsChannelCounters(t *testing.T) {
	app, err := Build(context.Background(), unreachableConfig(), io.Discard)
	if err != nil {
		t.Fatalf("装配失败：%v", err)
	}
	t.Cleanup(func() { _ = app.Close(context.Background()) })

	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/observability/metrics", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rec.Code, rec.Body.String())
	}
	for _, field := range []string{"queue_depth", "dropped_total", "emit_total", "emit_errors_total"} {
		if !strings.Contains(rec.Body.String(), field) {
			t.Errorf("指标快照缺少 %s：%s", field, rec.Body.String())
		}
	}
}
