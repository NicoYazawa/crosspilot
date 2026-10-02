package http

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/NicoYazawa/crosspilot/internal/config"
	"github.com/NicoYazawa/crosspilot/internal/presentation/agui"
	presauth "github.com/NicoYazawa/crosspilot/internal/presentation/auth"
	"github.com/NicoYazawa/crosspilot/internal/presentation/commerce"
	preobs "github.com/NicoYazawa/crosspilot/internal/presentation/observability"
)

// Deps 是组装路由所需的依赖。
//
// 每个子应用为 nil 表示「这个部署没有这一块」——但 nil 只应出现在测试装配里。
// 容器层必须把所有子应用都接上，否则进程会安静地只提供 /health，调用方看到
// 一片 404 却不知道是路由没挂还是路径写错。
type Deps struct {
	Config *config.Config
	Logger *slog.Logger
	// Checks 是 /health 要探测的外部依赖，按传入顺序逐项检查。
	Checks []Check
	// Auth 是身份校验配置。Secret 为空表示这一层未启用（开发期）。
	Auth presauth.Config
	// AGUI 是 AG-UI 子应用的依赖；为 nil 时不挂载。
	AGUI *agui.Deps
	// Commerce 是交易链路 handler；为 nil 时不挂载。
	Commerce *commerce.Handler
	// Observability 是可观测三件套 handler；为 nil 时不挂载。
	Observability *preobs.Handler
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

	// 身份链的顺序是「先解析会话，再校验令牌，最后兜底 demo 身份」。
	// 会话解析必须在令牌校验之前：X-Session-ID 是普通的请求头，与令牌无关，
	// 提前解析能让后面所有中间件都看得到它。
	identity := []func(http.Handler) http.Handler{presauth.SessionHeader()}
	if len(deps.Auth.Secret) > 0 {
		identity = append(identity, presauth.Middleware(deps.Auth))
	} else {
		// 未配置密钥时不放行匿名请求，而是补一个固定的 demo 身份：没有买家
		// 标识的请求在交易链路上会被一律 401，那样开发环境根本跑不通；
		// 而「静默放行」又会让一个忘配密钥的生产环境看起来一切正常。
		// 补 demo 身份把这件事变成可见的、可断言的默认值。
		identity = append(identity, presauth.DemoIdentity(demoBuyerID, demoSessionID))
	}
	identity = append(identity, presauth.RequireBuyer(deps.Logger))

	if deps.Commerce != nil {
		router.Mount(commercePrefix, chain(deps.Commerce.Routes(), identity...))
	}

	if deps.AGUI != nil {
		// AG-UI 与交易链路共用同一套身份：确认单的决议走 /commerce/confirmations，
		// 两条链路必须认同一个买家，否则「谁批准了这张单」会有两个答案。
		router.Mount(aguiPrefix, chain(agui.Routes(*deps.AGUI), identity...))
	}

	if deps.Observability != nil {
		// 可观测三件套是内部接口，不挂买家身份：它们面向的是运营与开发，
		// 而不是某个买家的会话。鉴权由上游网关负责（部署层面）。
		router.Mount(observabilityPrefix, deps.Observability.Routes())
	}

	router.NotFound(errorHandler(deps.Logger, http.StatusNotFound, "not_found"))
	router.MethodNotAllowed(errorHandler(deps.Logger, http.StatusMethodNotAllowed, "method_not_allowed"))

	return router
}

// 各子应用的挂载点（附录 B）。
//
// 前缀写在这里而不是写在各子包内部：chi 对同一个挂载路径只允许挂一次，
// 三个子应用各自带前缀去挂 "/" 会在第二个上 panic。挂载点集中在一处，
// 也让「这张路由表长什么样」可以在一个文件里读完。
const (
	commercePrefix      = "/commerce"
	aguiPrefix          = "/commerce/ag-ui"
	observabilityPrefix = "/observability"
)

// 未配置鉴权时使用的 demo 身份。
//
// 常量而不是随机值：开发与 E2E 断言需要它稳定，随机买家会让「刚才下的单
// 去哪了」变成每次都不一样的问题。
const (
	demoBuyerID   = "demo-buyer"
	demoSessionID = "demo-session"
)

// chain 按给定顺序把中间件套在 handler 上。
//
// 第一个参数是最外层——与 chi 的 Use 语义一致，这样写出来的顺序和读到的
// 顺序是同向的，不用在脑子里做一次翻转。
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// errorHandler 统一错误响应的形状，未命中路由与不支持的方法都走这里。
func errorHandler(logger *slog.Logger, status int, code string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, status, map[string]string{"error": code}, logger)
	}
}
