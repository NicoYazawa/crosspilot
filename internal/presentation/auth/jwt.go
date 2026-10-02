// Package auth 提供接口层的身份校验与身份传递。
//
// 为什么自己写 HS256 校验而不用现成库：附录 C#5 要求的是「键集精确」——
// header 只能有 {alg, typ}，claims 只能有 {sub, iat, exp, iss, aud}，多一个
// 键或少一个键都必须 401。主流 JWT 库的默认行为是「忽略不认识的声明」，
// 正好与这条契约相反；用库反而要再叠一层白名单校验，不如把校验点写在一处。
//
// 另一个理由是算法混淆：库通常按 header.alg 分派校验逻辑，把 alg 从 HS256
// 改成 none 或 RS256 是经典攻击面。这里的 alg 是拿常量比对的，不存在分派。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// 解析与校验过程中可能出现的失败原因。
//
// 全部收敛成一个对外的 ErrUnauthorized：把「签名不对」和「claim 过期」区分开
// 告诉调用方，等于免费提供了令牌探测的反馈信道。原因只进日志。
var (
	// ErrUnauthorized 是鉴权失败的统一出口。
	ErrUnauthorized = errors.New("auth: 身份验证失败")
)

// 校验用常量。
const (
	headerAuthorization = "Authorization"
	bearerPrefix        = "Bearer "

	// algHS256 是本服务唯一接受的签名算法。
	algHS256 = "HS256"

	// maxClockSkew 是 iat / exp 允许的时钟偏差。
	//
	// 太宽松等于放开重放窗口，太严格会让「客户端快一秒」变成随机 401。
	// 1 分钟覆盖常见的 NTP 漂移，又不足以让一个过期令牌复活。
	maxClockSkew = time.Minute
)

// requiredClaimKeys 是唯一被接受的 claim 键集（附录 C#5）。
//
// 用 map 而不是逐个字段判断，是因为契约要求的是「集合相等」而不是「字段存在」：
// 多出任何一个未在契约里的键都意味着签发方与校验方对「身份」的定义已经漂移。
var requiredClaimKeys = map[string]struct{}{
	"sub": {},
	"iat": {},
	"exp": {},
	"iss": {},
	"aud": {},
}

// claims 是校验通过的令牌内容。
type claims struct {
	Subject   string
	Issuer    string
	Audience  string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Config 是中间件的构造参数。
type Config struct {
	// Secret 是 HS256 的共享密钥。为空表示这一层未启用（开发期）。
	Secret []byte
	// Typ 是 header.typ 的期望值，必须精确匹配。
	Typ string
	// Logger 用于记录失败原因，可为 nil。
	Logger *slog.Logger
	// Clock 提供当前时间；为 nil 时用 time.Now。
	Clock func() time.Time
}

// Middleware 校验 Authorization: Bearer <jwt>。
//
// Secret 为空时放行：配置缺失只代表「这一层还没启用」，把决策权留给部署者。
// 注意这与「用空密钥校验」是两回事——后者会让任何签名都通过，绝不能做。
func Middleware(cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(cfg.Secret) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			raw, ok := bearerToken(r)
			if !ok {
				reject(w, cfg.Logger, r, "缺少 Bearer 令牌")
				return
			}

			c, err := verify(raw, cfg.Secret, cfg.Typ, cfg.Clock)
			if err != nil {
				reject(w, cfg.Logger, r, err.Error())
				return
			}

			next.ServeHTTP(w, r.WithContext(withClaims(r.Context(), c)))
		})
	}
}

// verify 校验令牌并返回其 claims。
//
// 顺序是有意为之：先定形状（键集、算法、类型），再验签名，最后看时效。
// 签名没验通过之前读 claims 是不可靠的，但形状检查不需要密钥、代价极低，
// 放在最前面能把绝大多数畸形输入挡在 HMAC 计算之外。
func verify(raw string, secret []byte, typ string, clock func() time.Time) (claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return claims{}, fmt.Errorf("%w: 令牌不是三段式", ErrUnauthorized)
	}

	headerJSON, err := decodeSegment(parts[0])
	if err != nil {
		return claims{}, fmt.Errorf("%w: header 不是合法 base64: %w", ErrUnauthorized, err)
	}
	payloadJSON, err := decodeSegment(parts[1])
	if err != nil {
		return claims{}, fmt.Errorf("%w: payload 不是合法 base64: %w", ErrUnauthorized, err)
	}

	if err := checkHeader(headerJSON, typ); err != nil {
		return claims{}, err
	}
	if err := checkSignature(parts[0]+"."+parts[1], parts[2], secret); err != nil {
		return claims{}, err
	}
	return checkClaims(payloadJSON, clock)
}

// checkHeader 要求 header 的键集恰好是 {alg, typ}，且取值符合契约。
func checkHeader(raw []byte, typ string) error {
	var h map[string]json.RawMessage
	if err := json.Unmarshal(raw, &h); err != nil {
		return fmt.Errorf("%w: header 不是 JSON 对象: %w", ErrUnauthorized, err)
	}
	if len(h) != 2 {
		return fmt.Errorf("%w: header 键集必须是 {alg,typ}，实际 %d 个键", ErrUnauthorized, len(h))
	}
	var alg, gotTyp string
	if err := decodeStringField(h, "alg", &alg); err != nil {
		return err
	}
	if err := decodeStringField(h, "typ", &gotTyp); err != nil {
		return err
	}
	if alg != algHS256 {
		return fmt.Errorf("%w: alg 必须是 %s，实际 %q", ErrUnauthorized, algHS256, alg)
	}
	if gotTyp != typ {
		return fmt.Errorf("%w: typ 必须是 %q，实际 %q", ErrUnauthorized, typ, gotTyp)
	}
	return nil
}

// checkSignature 用常数时间比较重算的 HMAC。
//
// 常数时间不是洁癖：逐字节短路比较会把「前几个字节对上了」编码进响应耗时，
// 让攻击者可以逐字节把签名试出来。
func checkSignature(signingInput, sigSegment string, secret []byte) error {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	expected := mac.Sum(nil)

	provided, err := base64.RawURLEncoding.DecodeString(sigSegment)
	if err != nil {
		return fmt.Errorf("%w: 签名不是合法 base64url: %w", ErrUnauthorized, err)
	}
	if subtle.ConstantTimeCompare(expected, provided) != 1 {
		return fmt.Errorf("%w: 签名不匹配", ErrUnauthorized)
	}
	return nil
}

// checkClaims 要求 claims 的键集恰好是契约规定的五个，且取值自洽。
func checkClaims(raw []byte, clock func() time.Time) (claims, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return claims{}, fmt.Errorf("%w: payload 不是 JSON 对象: %w", ErrUnauthorized, err)
	}
	if len(m) != len(requiredClaimKeys) {
		return claims{}, fmt.Errorf("%w: claim 键集必须是 %d 个，实际 %d 个", ErrUnauthorized, len(requiredClaimKeys), len(m))
	}
	for k := range m {
		if _, ok := requiredClaimKeys[k]; !ok {
			return claims{}, fmt.Errorf("%w: 出现契约外的 claim %q", ErrUnauthorized, k)
		}
	}

	var c claims
	if err := decodeStringField(m, "sub", &c.Subject); err != nil {
		return claims{}, err
	}
	if err := decodeStringField(m, "iss", &c.Issuer); err != nil {
		return claims{}, err
	}
	// strict_aud：aud 必须是单个字符串。数组形式（RFC 7519 允许）在这里被拒绝，
	// 因为「多受众」会让「这个令牌是发给谁的」失去唯一答案。
	if err := decodeStringField(m, "aud", &c.Audience); err != nil {
		return claims{}, fmt.Errorf("%w（strict_aud 要求 aud 为字符串）", err)
	}
	if c.Subject == "" {
		return claims{}, fmt.Errorf("%w: sub 不能为空", ErrUnauthorized)
	}

	var iat, exp int64
	if err := decodeInt64Field(m, "iat", &iat); err != nil {
		return claims{}, err
	}
	if err := decodeInt64Field(m, "exp", &exp); err != nil {
		return claims{}, err
	}

	now := time.Now
	if clock != nil {
		now = clock
	}
	at := now()

	c.IssuedAt = time.Unix(iat, 0).UTC()
	c.ExpiresAt = time.Unix(exp, 0).UTC()

	if at.After(c.ExpiresAt.Add(maxClockSkew)) {
		return claims{}, fmt.Errorf("%w: 令牌已过期", ErrUnauthorized)
	}
	if c.IssuedAt.After(at.Add(maxClockSkew)) {
		return claims{}, fmt.Errorf("%w: 令牌签发时间在未来", ErrUnauthorized)
	}
	return c, nil
}

// --- 解码辅助 -------------------------------------------------------------
//
// 每个字段单独解码而不是一次性 Unmarshal 到结构体：结构体解码会静默忽略
// 契约外的键和多类型，而这里恰恰要求「多一个键就失败」。

func decodeSegment(seg string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(seg)
}

func decodeStringField(m map[string]json.RawMessage, key string, dst *string) error {
	raw, ok := m[key]
	if !ok {
		return fmt.Errorf("%w: 缺少 claim %q", ErrUnauthorized, key)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%w: claim %q 不是字符串", ErrUnauthorized, key)
	}
	return nil
}

func decodeInt64Field(m map[string]json.RawMessage, key string, dst *int64) error {
	raw, ok := m[key]
	if !ok {
		return fmt.Errorf("%w: 缺少 claim %q", ErrUnauthorized, key)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%w: claim %q 不是整数时间戳", ErrUnauthorized, key)
	}
	return nil
}

func bearerToken(r *http.Request) (string, bool) {
	v := strings.TrimSpace(r.Header.Get(headerAuthorization))
	if !strings.HasPrefix(v, bearerPrefix) {
		return "", false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(v, bearerPrefix))
	return tok, tok != ""
}

func reject(w http.ResponseWriter, logger *slog.Logger, r *http.Request, reason string) {
	if logger != nil {
		logger.WarnContext(r.Context(), "鉴权失败",
			slog.String("reason", reason),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
		)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}
