package auth

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
)

// headerSessionID 携带会话标识。
//
// 为什么 session 走 header 而不是 JWT claim：附录 C#5 把 claim 键集钉死为
// {sub, iat, exp, iss, aud}，多一个 session_id 就违约。会话是「这一次浏览」
// 的上下文，本来也不属于「你是谁」这份长期身份——把它塞进令牌会让同一个
// 买家开两个标签页时被迫共用一个会话。
const headerSessionID = "X-Session-ID"

type identityKey struct{}

// withClaims 把令牌内容放进 context。
func withClaims(ctx context.Context, c claims) context.Context {
	return context.WithValue(ctx, identityKey{}, c)
}

// WithBuyer 把买家身份放进 context。
//
// 供中间件之外的场景使用（测试直接构造请求上下文，或未来从别的凭据解析出
// 身份）。处理器只认 context，不关心身份是哪来的。
func WithBuyer(ctx context.Context, buyerID string) context.Context {
	return context.WithValue(ctx, identityKey{}, claims{Subject: buyerID})
}

// sessionKey 单独存会话，避免污染 claims 的语义。
type sessionKey struct{}

// WithSession 把会话标识放进 context。
func WithSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, sessionKey{}, sessionID)
}

// BuyerFrom 取出当前请求的买家标识；未通过鉴权时返回空串。
func BuyerFrom(ctx context.Context) string {
	if c, ok := ctx.Value(identityKey{}).(claims); ok {
		return c.Subject
	}
	return ""
}

// SessionFrom 取出当前请求的会话标识；未提供时返回空串。
func SessionFrom(ctx context.Context) string {
	if s, ok := ctx.Value(sessionKey{}).(string); ok {
		return s
	}
	return ""
}

// SessionHeader 读取会话头的中间件。
//
// 它不做校验——「这个端点是否需要会话」是各端点自己的事，由 RequireSession
// 表达。把两件事拆开，是因为「没带会话」对只读端点无所谓，对下单则是错误。
func SessionHeader() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if sid := strings.TrimSpace(r.Header.Get(headerSessionID)); sid != "" {
				r = r.WithContext(WithSession(r.Context(), sid))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// DemoIdentity 在请求没有身份时补上一个固定身份。
//
// 只在未配置 JWT 密钥时挂载（开发期）。它必须让真身份优先：一旦请求已经
// 带了 sub，这里不再覆盖——否则一个配错密钥的环境会把所有调用者都变成
// 同一个 demo 买家，而那种错误在日志里看不出来。
//
// 它不创建会话：会话由 X-Session-ID 决定，缺省时用 fallbackSession。
func DemoIdentity(buyerID, fallbackSession string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			if BuyerFrom(ctx) == "" && buyerID != "" {
				ctx = WithBuyer(ctx, buyerID)
			}
			if SessionFrom(ctx) == "" && fallbackSession != "" {
				ctx = WithSession(ctx, fallbackSession)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireBuyer 拒绝没有买家身份的请求（附录 B 的 B 标记）。
func RequireBuyer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if BuyerFrom(r.Context()) == "" {
				reject(w, logger, r, "缺少买家身份")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireSession 拒绝没有会话标识的请求（附录 B 的 S 标记）。
//
// 返回 400 而不是 401：调用方身份没问题，缺的是这次请求的参数。用 401 会让
// 客户端误以为该去刷新令牌，而重试同一个请求永远不会成功。
func RequireSession(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if SessionFrom(r.Context()) == "" {
				if logger != nil {
					logger.WarnContext(r.Context(), "请求缺少会话标识",
						slog.String("path", r.URL.Path))
				}
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"missing_session"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
