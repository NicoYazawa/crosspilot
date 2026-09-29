package http

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/NicoYazawa/crosspilot/internal/config"
)

// Deps 是组装路由所需的依赖。
type Deps struct {
	Config *config.Config
	Logger *slog.Logger
	// Checks 是 /health 要探测的外部依赖，按传入顺序逐项检查。
	Checks []Check
}

// NewRouter 组装路由表。
//
// 中间件顺序即执行顺序：请求标识最先建立，后续所有日志才带得上它；
// 恢复中间件包住业务处理器，保证 panic 不会逃逸到监听循环。
func NewRouter(deps Deps) http.Handler {
	router := chi.NewRouter()

	router.Use(RequestID)
	router.Use(Recoverer(deps.Logger))
	router.Use(CORS(deps.Config.HTTP.CORSOrigins))

	router.Get("/health", HealthHandler(deps.Logger, deps.Checks...))
	router.Get("/health/live", LiveHandler(deps.Logger))

	router.NotFound(errorHandler(deps.Logger, http.StatusNotFound, "not_found"))
	router.MethodNotAllowed(errorHandler(deps.Logger, http.StatusMethodNotAllowed, "method_not_allowed"))

	return router
}

// errorHandler 统一错误响应的形状，未命中路由与不支持的方法都走这里。
func errorHandler(logger *slog.Logger, status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, status, map[string]string{"error": code}, logger)
	}
}
