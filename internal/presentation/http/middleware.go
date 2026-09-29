package http

import (
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/NicoYazawa/crosspilot/internal/infra/logging"
)

// 请求标识的请求头与响应头名，沿用通用约定。
const headerRequestID = "X-Request-Id"

// preflightMaxAge 是预检结果在浏览器侧的缓存时长。
const preflightMaxAge = 10 * time.Minute

// RequestID 为每个请求分配标识，写入 context 并回写到响应头。
//
// 调用方自带标识时沿用调用方的，便于跨服务串联同一条链路。
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(headerRequestID))
		if id == "" {
			id = rand.Text()
		}
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

// Recoverer 兜住处理器里的 panic，避免单个请求打挂整个进程。
//
// 堆栈只进日志，不进响应体：堆栈会暴露内部结构与路径，对调用方没有用处。
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					return
				}
				// http.ErrAbortHandler 是标准库约定的静默中止信号，按惯例继续上抛。
				// 用 errors.Is 而不是 ==：被包装过的同一个信号同样要放行。
				if err, ok := recovered.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(recovered)
				}
				if logger != nil {
					logger.ErrorContext(r.Context(), "请求处理 panic",
						slog.Any("panic", recovered),
						slog.String("method", r.Method),
						slog.String("path", r.URL.Path),
						slog.String("stack", string(debug.Stack())))
				}
				writeJSON(w, http.StatusInternalServerError,
					map[string]string{"error": "internal"}, logger)
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// CORS 按允许列表放行跨域请求。
//
// 列表为空表示不下发任何跨域头，浏览器保持同源策略的默认拒绝。这里不做
// 通配放行：那在开发期省事，上线后等于把接口对所有站点开放。
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		if origin = strings.TrimSpace(origin); origin != "" {
			allowed[origin] = struct{}{}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if _, ok := allowed[origin]; !ok {
				next.ServeHTTP(w, r)
				return
			}

			// 回显具体来源而不是 *：带上凭据的请求不允许通配
			headers := w.Header()
			headers.Set("Access-Control-Allow-Origin", origin)
			headers.Add("Vary", "Origin")

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				headers.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
				if requested := r.Header.Get("Access-Control-Request-Headers"); requested != "" {
					headers.Set("Access-Control-Allow-Headers", requested)
				}
				headers.Set("Access-Control-Max-Age", strconv.Itoa(int(preflightMaxAge.Seconds())))
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
