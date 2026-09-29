package http

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// up 表示探测正常，down 表示探测失败。
func up(context.Context) error   { return nil }
func down(context.Context) error { return errors.New("连接被拒绝") }

// discardLogger 让被测代码的日志不污染测试输出。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardWriter{}, nil))
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestHealthHandlerAllUp(t *testing.T) {
	handler := HealthHandler(discardLogger(),
		Check{Name: "db", Probe: up},
		Check{Name: "redis", Probe: up},
	)

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}

	body := rec.Body.String()
	for _, want := range []string{`"status":"ok"`, `"db":"up"`, `"redis":"up"`} {
		if !strings.Contains(body, want) {
			t.Errorf("响应体缺少 %s：%s", want, body)
		}
	}
}

// TestHealthHandlerDatabaseDown 覆盖 A2：数据库断开时必须是 503 且指明 db 掉了。
// 这是 P0 最关键的一条验收——启动不依赖数据库，但就绪必须如实反映数据库状态。
func TestHealthHandlerDatabaseDown(t *testing.T) {
	handler := HealthHandler(discardLogger(),
		Check{Name: "db", Probe: down},
		Check{Name: "redis", Probe: up},
	)

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"db":"down"`) {
		t.Errorf("响应体未指明 db 不可用：%s", body)
	}
	if !strings.Contains(body, `"status":"down"`) {
		t.Errorf("响应体未把整体状态标记为 down：%s", body)
	}
	// 单项失败不应连累其他项的报告
	if !strings.Contains(body, `"redis":"up"`) {
		t.Errorf("响应体丢失了 redis 的结果：%s", body)
	}
}

func TestHealthHandlerNoChecks(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthHandler(discardLogger())(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"checks":{}`) {
		t.Errorf("响应体 = %s，期望空的 checks 对象", got)
	}
}

func TestHealthHandlerNilLogger(t *testing.T) {
	// 探测失败时不应因为 logger 为 nil 而 panic
	rec := httptest.NewRecorder()
	HealthHandler(nil, Check{Name: "db", Probe: down})(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码 = %d，期望 503", rec.Code)
	}
}

func TestHealthHandlerSetsContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	HealthHandler(discardLogger())(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestLiveHandlerIgnoresDependencies(t *testing.T) {
	rec := httptest.NewRecorder()
	LiveHandler(discardLogger())(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：存活探测不应受依赖影响", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"status":"ok"`) {
		t.Errorf("响应体 = %s", got)
	}
}

func TestWriteJSONMarshalFailure(t *testing.T) {
	// channel 无法序列化，应当在写响应头之前就失败
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, make(chan int), discardLogger())

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
}

// TestHealthHandlerBoundsProbeTime 确认单个探测卡住不会拖垮整个健康检查。
//
// 依赖失联时最需要健康检查快速给出结论，而失联恰恰是探测最容易变慢的时刻。
// 这项保证让 /health 的响应时间有上限，与具体依赖的表现无关。
func TestHealthHandlerBoundsProbeTime(t *testing.T) {
	// 一个永远不返回的探测，且不理会 ctx
	stuck := func(context.Context) error {
		time.Sleep(time.Minute)
		return nil
	}

	handler := HealthHandler(discardLogger(),
		Check{Name: "db", Probe: up},
		Check{Name: "redis", Probe: stuck},
	)

	rec := httptest.NewRecorder()

	start := time.Now()
	handler(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	elapsed := time.Since(start)

	if elapsed > probeBudget+250*time.Millisecond {
		t.Fatalf("健康检查耗时 %v，超出预算 %v", elapsed, probeBudget)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", rec.Code)
	}
	// 正常的项仍要如实报告：一项卡住不该掩盖其他项的真实状态
	body := rec.Body.String()
	for _, want := range []string{`"status":"down"`, `"db":"up"`, `"redis":"down"`} {
		if !strings.Contains(body, want) {
			t.Errorf("响应体缺少 %s：%s", want, body)
		}
	}
}

// TestHealthHandlerHonoursRequestCancellation 确认调用方断开后不再空等。
func TestHealthHandlerHonoursRequestCancellation(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	blocking := func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	handler := HealthHandler(discardLogger(), Check{Name: "db", Probe: blocking})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503", rec.Code)
	}
}
