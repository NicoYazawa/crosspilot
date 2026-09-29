package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

func testDeps() Deps {
	return Deps{
		Config: &config.Config{
			HTTP: config.HTTPConfig{CORSOrigins: []string{"https://shop.example.com"}},
		},
		Logger: discardLogger(),
		Checks: []Check{
			{Name: "db", Probe: up},
			{Name: "redis", Probe: up},
		},
	}
}

func TestRouterHealthEndpoint(t *testing.T) {
	router := NewRouter(testDeps())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"db":"up"`) {
		t.Errorf("响应体 = %s", rec.Body.String())
	}
}

func TestRouterLiveEndpoint(t *testing.T) {
	router := NewRouter(testDeps())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", rec.Code)
	}
}

func TestRouterUnknownPathReturnsJSON(t *testing.T) {
	router := NewRouter(testDeps())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rec.Code)
	}
	if got := rec.Body.String(); got != `{"error":"not_found"}` {
		t.Errorf("响应体 = %s", got)
	}
}

func TestRouterMethodNotAllowed(t *testing.T) {
	router := NewRouter(testDeps())

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/health", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("状态码 = %d，期望 405", rec.Code)
	}
	if got := rec.Body.String(); got != `{"error":"method_not_allowed"}` {
		t.Errorf("响应体 = %s", got)
	}
}

func TestRouterAppliesRequestIDToEveryRoute(t *testing.T) {
	router := NewRouter(testDeps())

	for _, path := range []string{"/health", "/health/live", "/nope"} {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if rec.Header().Get(headerRequestID) == "" {
			t.Errorf("%s 的响应缺少请求标识", path)
		}
	}
}

func TestRouterAppliesCORSFromConfig(t *testing.T) {
	router := NewRouter(testDeps())

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://shop.example.com" {
		t.Errorf("Allow-Origin = %q", got)
	}
}
