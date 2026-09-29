package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NicoYazawa/crosspilot/internal/infra/logging"
)

// echoRequestID 把 context 里的请求标识回写进 body，便于断言中间件确实写进去了。
func echoRequestID(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte(logging.RequestID(r.Context())))
}

func TestRequestIDGeneratesWhenAbsent(t *testing.T) {
	rec := httptest.NewRecorder()
	RequestID(http.HandlerFunc(echoRequestID)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	header := rec.Header().Get(headerRequestID)
	body := rec.Body.String()

	if header == "" {
		t.Fatal("响应头里没有请求标识")
	}
	if body != header {
		t.Errorf("context 里的标识 %q 与响应头 %q 不一致", body, header)
	}
}

func TestRequestIDKeepsCallerSupplied(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(headerRequestID, "caller-supplied-id")

	rec := httptest.NewRecorder()
	RequestID(http.HandlerFunc(echoRequestID)).ServeHTTP(rec, req)

	if got := rec.Body.String(); got != "caller-supplied-id" {
		t.Errorf("body = %q，期望沿用调用方传入的标识", got)
	}
}

func TestRequestIDIgnoresBlankHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(headerRequestID, "   ")

	rec := httptest.NewRecorder()
	RequestID(http.HandlerFunc(echoRequestID)).ServeHTTP(rec, req)

	if got := rec.Body.String(); got == "" || strings.TrimSpace(got) == "" {
		t.Errorf("空白标识应当被替换为新生成的标识，实际 %q", got)
	}
}

func TestRequestIDIsUnique(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		rec := httptest.NewRecorder()
		RequestID(http.HandlerFunc(echoRequestID)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if id := rec.Body.String(); seen[id] {
			t.Fatalf("请求标识重复：%q", id)
		} else {
			seen[id] = true
		}
	}
}

func TestRecovererTurnsPanicIntoInternalError(t *testing.T) {
	handler := Recoverer(discardLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("处理器炸了")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}

	body := rec.Body.String()
	if strings.Contains(body, "处理器炸了") || strings.Contains(body, "goroutine") {
		t.Errorf("响应体泄漏了 panic 细节或堆栈：%s", body)
	}
}

func TestRecovererPassesThroughNormalRequests(t *testing.T) {
	handler := Recoverer(discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("状态码 = %d，期望 418", rec.Code)
	}
}

func TestRecovererRepanicsAbortHandler(t *testing.T) {
	// ErrAbortHandler 是标准库约定的静默中止信号，必须继续上抛
	handler := Recoverer(discardLogger())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		recovered, ok := recover().(error)
		if !ok || !errors.Is(recovered, http.ErrAbortHandler) {
			t.Errorf("panic = %v，期望 ErrAbortHandler 被重新抛出", recovered)
		}
	}()
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
}

func TestRecovererNilLogger(t *testing.T) {
	handler := Recoverer(nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("状态码 = %d，期望 500", rec.Code)
	}
}

func TestCORSAllowedOrigin(t *testing.T) {
	handler := CORS([]string{"https://shop.example.com"})(http.HandlerFunc(echoRequestID))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://shop.example.com" {
		t.Errorf("Allow-Origin = %q，期望回显具体来源", got)
	}
	if got := rec.Header().Values("Vary"); len(got) == 0 || got[0] != "Origin" {
		t.Errorf("Vary = %v，期望含 Origin", got)
	}
}

func TestCORSDisallowedOriginGetsNoHeaders(t *testing.T) {
	handler := CORS([]string{"https://shop.example.com"})(http.HandlerFunc(echoRequestID))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://evil.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q，未列入允许列表的来源不应放行", got)
	}
	// 请求本身仍然照常处理，跨域限制由浏览器执行
	if rec.Code != http.StatusOK {
		t.Errorf("状态码 = %d，期望照常处理", rec.Code)
	}
}

func TestCORSEmptyAllowlistRejectsEveryOrigin(t *testing.T) {
	handler := CORS(nil)(http.HandlerFunc(echoRequestID))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q，空允许列表不应放行任何来源", got)
	}
}

func TestCORSPreflight(t *testing.T) {
	handler := CORS([]string{"https://shop.example.com"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("预检请求不应进入业务处理器")
	}))

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type,authorization")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("状态码 = %d，期望 204", rec.Code)
	}

	headers := rec.Header()
	if got := headers.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
		t.Errorf("Allow-Methods = %q", got)
	}
	if got := headers.Get("Access-Control-Allow-Headers"); got != "content-type,authorization" {
		t.Errorf("Allow-Headers = %q，期望回显请求的头", got)
	}
	if got := headers.Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("Max-Age = %q，期望 600", got)
	}
}

func TestCORSOptionsWithoutPreflightHeaderIsNormalRequest(t *testing.T) {
	// 不带 Access-Control-Request-Method 的 OPTIONS 不是预检，应当照常分发
	handler := CORS([]string{"https://shop.example.com"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/", nil)
	req.Header.Set("Origin", "https://shop.example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("状态码 = %d，期望 418", rec.Code)
	}
}

func TestCORSBlankOriginIgnored(t *testing.T) {
	handler := CORS([]string{"", "   ", "https://shop.example.com"})(http.HandlerFunc(echoRequestID))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Origin", "")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// 空来源不在允许列表里（空白项已被丢弃）
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin = %q", got)
	}
}
