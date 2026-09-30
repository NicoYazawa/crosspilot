package http

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/NicoYazawa/crosspilot/internal/config"
	"github.com/NicoYazawa/crosspilot/internal/presentation/agui"
)

// Deps 是组装路由所需的依赖。
type Deps struct {
	Config *config.Config
	Logger *slog.Logger
	// Checks 是 /health 要探测的外部依赖，按传入顺序逐项检查。
	Checks []Check
	// AGUI 是 AG-UI 子应用的依赖；为 nil 时不挂载 /agui 路由。
	AGUI *agui.Deps
	// HMACSecret 是 AG-UI 鉴权密钥；空表示该中间件跳过（开发期）。
	HMACSecret []byte
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

	if deps.AGUI != nil {
		// AG-UI 子应用挂在自己的中间件链里（HMAC + RequestID 之外的）。
		router.Mount("/", aguiAuthMount(deps.HMACSecret, deps.Logger, agui.Routes(*deps.AGUI)))
	}

	router.NotFound(errorHandler(deps.Logger, http.StatusNotFound, "not_found"))
	router.MethodNotAllowed(errorHandler(deps.Logger, http.StatusMethodNotAllowed, "method_not_allowed"))

	return router
}

// aguiAuthMount 把 HMAC 鉴权包到 AG-UI 子路由上。
//
// 注意：chi.Mount 会把 /agui 前缀按子路由器规则去掉；HMACAuth 校验的是
// URL.Path，子路由器内部 path 仍是 /runs/...，HMAC 计算用的是客户端实际
// 看到的完整路径 /agui/runs/...——所以密钥计算时必须用客户端 URL.Path。
func aguiAuthMount(secret []byte, logger *slog.Logger, next http.Handler) http.Handler {
	return agui.HMACAuth(secret, logger)(next)
}

// errorHandler 统一错误响应的形状，未命中路由与不支持的方法都走这里。
func errorHandler(logger *slog.Logger, status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, status, map[string]string{"error": code}, logger)
	}
}
