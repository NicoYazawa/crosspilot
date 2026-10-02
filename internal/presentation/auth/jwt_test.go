package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 本文件是 package auth 的内部测试（而不是 auth_test）：只有站在包内才能
// 直接拿到 maxClockSkew 这类未导出常量来钉住「恰好偏移一分钟」的边界，
// 也才能在不解一组 HTTP 请求的情况下直接验证 verify/checkClaims。

// testSecret 是一把只在测试里使用的 HS256 密钥。
var testSecret = []byte("unit-test-secret-0123456789abcdef")

// testTyp 是测试用的 header typ 取值。
//
// 刻意不写成生产的默认值 `crosspilot-access+jwt`（见 config.go 的 AUTH_JWT_TYP）：
// typ 由 Config 传入、校验逻辑不认死值，用一个「一眼看出是测试」的值才能证明
// 中间件真的在读配置，而不是把某个常量硬编码进比较里。
// 用例都显式传 Config{Typ: testTyp}，与生产的差异不会影响结论。
const testTyp = "JWT"

// fixedNow 是所有时间相关用例的「现在」，用可注入的 Clock 控制，
// 因此测试里没有任何 time.Sleep——时间边界必须是确定性的。
var fixedNow = time.Unix(1_700_000_000, 0).UTC()

func fixedClock(at time.Time) func() time.Time { return func() time.Time { return at } }

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// validHeader 是契约允许的唯一 header 形状：恰好 {alg, typ}。
func validHeader() map[string]any { return map[string]any{"alg": "HS256", "typ": testTyp} }

// validPayload 是一份键集恰好为 {sub, iat, exp, iss, aud} 的自洽 claims。
func validPayload(now time.Time) map[string]any {
	return map[string]any{
		"sub": "buyer-1",
		"iat": now.Add(-10 * time.Second).Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
		"iss": "crosspilot",
		"aud": "crosspilot-api",
	}
}

// signToken 按 HS256 组装一个三段式令牌。header/payload 以 map 传入，
// 让调用方可以单独构造「多一个键」「alg 换了」这类畸形输入——
// 用一份「正常 payload 改一处」的构造方式，测试意图才一眼可见。
func signToken(t *testing.T, secret []byte, header, payload map[string]any) string {
	t.Helper()
	signingInput, sig := signParts(t, secret, header, payload)
	return signingInput + "." + b64(sig)
}

// signParts 返回签名输入与原始签名，供需要篡改签名的用例复用。
func signParts(t *testing.T, secret []byte, header, payload map[string]any) (string, []byte) {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("序列化 header 失败：%v", err)
	}
	p, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化 payload 失败：%v", err)
	}
	signingInput := b64(h) + "." + b64(p)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	return signingInput, mac.Sum(nil)
}

// rawToken 用调用方给定的签名段拼令牌，用于 none / RS256 这类「签名本来就不该被算」的攻击。
func rawToken(t *testing.T, header, payload map[string]any, sigSegment string) string {
	t.Helper()
	h, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("序列化 header 失败：%v", err)
	}
	p, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化 payload 失败：%v", err)
	}
	return b64(h) + "." + b64(p) + "." + sigSegment
}

// mwResult 是一次中间件执行的可观测结果。
type mwResult struct {
	rec     *httptest.ResponseRecorder
	called  bool
	buyerID string
	session string
}

// runMiddleware 把请求喂给中间件 + 下游，收集下游是否被调用、看到了什么身份。
//
// 同时断言「状态码」和「下游是否执行」：只看状态码会漏掉一类致命实现错误——
// 中间件放行了一个本该拒绝的令牌，但下游自己碰巧也返回 401，测试就会误判为通过。
func runMiddleware(cfg Config, setHeader bool, authValue string) mwResult {
	res := mwResult{}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		res.called = true
		res.buyerID = BuyerFrom(r.Context())
		res.session = SessionFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(cfg)(next)
	req := httptest.NewRequest(http.MethodGet, "/protected", nil)
	if setHeader {
		req.Header.Set(headerAuthorization, authValue)
	}
	res.rec = httptest.NewRecorder()
	h.ServeHTTP(res.rec, req)
	return res
}

// TestMiddleware_合法令牌_通过并注入买家身份 覆盖正常路径。
//
// 只断言 200 是不够的：身份必须真的落到 context，否则后续所有按 sub 隔离
// 资源的处理器都会拿到空买家，把「查不到」和「越权」这两种情况混为一谈。
func TestMiddleware_合法令牌_通过并注入买家身份(t *testing.T) {
	tok := signToken(t, testSecret, validHeader(), validPayload(fixedNow))
	res := runMiddleware(Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)},
		true, "Bearer "+tok)

	if res.rec.Code != http.StatusOK {
		t.Fatalf("合法令牌应放行，状态码 = %d，body=%s", res.rec.Code, res.rec.Body.String())
	}
	if !res.called {
		t.Fatal("合法令牌必须调用下游处理器")
	}
	if res.buyerID != "buyer-1" {
		t.Errorf("BuyerFrom(ctx) = %q，期望 %q（身份未从令牌注入 context）", res.buyerID, "buyer-1")
	}
}

// TestMiddleware_签名被篡改_拒绝 守住「签名必须真的参与校验」。
//
// 篡改发生在签名段中间一个字符：该位置仍是合法 base64url，解码长度不变，
// 因此它只可能因「HMAC 对不上」被拒，而不是因为 base64 畸形——
// 这能区分「签名比较被实现成恒真」与「base64 解析失败」两种失败。
func TestMiddleware_签名被篡改_拒绝(t *testing.T) {
	tok := signToken(t, testSecret, validHeader(), validPayload(fixedNow))
	parts := strings.Split(tok, ".")
	sig := []byte(parts[2])
	mid := len(sig) / 2
	repl := byte('A')
	if sig[mid] == 'A' {
		repl = 'B'
	}
	sig[mid] = repl
	parts[2] = string(sig)
	tampered := strings.Join(parts, ".")

	res := runMiddleware(Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)},
		true, "Bearer "+tampered)

	if res.rec.Code != http.StatusUnauthorized {
		t.Fatalf("签名被改一个字符应 401，实际 %d，body=%s", res.rec.Code, res.rec.Body.String())
	}
	if res.called {
		t.Error("签名不匹配时绝不能调用下游处理器")
	}
}

// TestMiddleware_错误密钥签发_拒绝 守住「签名与密钥绑定」。
//
// 没有这条，一个「只要签名段非空就放行」的实现会漏过所有攻击者自签的令牌。
func TestMiddleware_错误密钥签发_拒绝(t *testing.T) {
	other := []byte("attacker-secret-9999999999999999")
	tok := signToken(t, other, validHeader(), validPayload(fixedNow))

	res := runMiddleware(Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)},
		true, "Bearer "+tok)

	if res.rec.Code != http.StatusUnauthorized {
		t.Fatalf("用错误密钥签的令牌应 401，实际 %d", res.rec.Code)
	}
	if res.called {
		t.Error("错误密钥的令牌绝不能调用下游处理器")
	}
}

// TestMiddleware_alg混淆_拒绝 守住算法混淆攻击面。
//
// alg=none 是经典绕过：某些库按 header.alg 分派校验，none 分支直接跳过验签。
// RS256 攻击则诱导库用公钥当 HMAC 密钥验签。这里的 alg 必须拿常量比对，
// 一旦实现改成「按 alg 分派」，这两条就会变红。
func TestMiddleware_alg混淆_拒绝(t *testing.T) {
	payload := validPayload(fixedNow)
	cfg := Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)}

	cases := []struct {
		name string
		tok  string
	}{
		// 最原始的 none 攻击：签名段干脆留空。
		{"alg=none 且签名为空", rawToken(t, map[string]any{"alg": "none", "typ": testTyp}, payload, "")},
		// 换个非空签名，排除「空签名被单独拦截」导致的假绿。
		{"alg=none 且签名非空", rawToken(t, map[string]any{"alg": "none", "typ": testTyp}, payload, b64([]byte("fake-sig")))},
		{"alg=RS256", rawToken(t, map[string]any{"alg": "RS256", "typ": testTyp}, payload, b64([]byte("fake-sig")))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runMiddleware(cfg, true, "Bearer "+tc.tok)
			if res.rec.Code != http.StatusUnauthorized {
				t.Fatalf("非 HS256 的 alg 必须 401，实际 %d，body=%s", res.rec.Code, res.rec.Body.String())
			}
			if res.called {
				t.Error("alg 不合规的令牌绝不能调用下游处理器")
			}
		})
	}
}

// TestMiddleware_header键集严格_多一个键即拒绝 覆盖附录 C#5 的「键集精确」。
//
// 多一个 kid 是常见做法（用密钥轮换），但契约里 header 只能有 {alg, typ}。
// 如果实现改成「大小 >= 2」或只判断字段存在，这条会红——那正是把
// 「签发方与校验方对 header 的定义已经漂移」放行的口子。
func TestMiddleware_header键集严格_多一个键即拒绝(t *testing.T) {
	header := map[string]any{"alg": "HS256", "typ": testTyp, "kid": "key-2"}
	tok := signToken(t, testSecret, header, validPayload(fixedNow))

	res := runMiddleware(Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)},
		true, "Bearer "+tok)

	if res.rec.Code != http.StatusUnauthorized {
		t.Fatalf("header 多出 kid 应 401，实际 %d", res.rec.Code)
	}
	if res.called {
		t.Error("header 键集不合规时绝不能调用下游处理器")
	}
}

// TestMiddleware_claims键集严格_多键或少键都拒绝 覆盖 claims 的键集精确。
//
// 多一个 scope 会让「这个令牌能做什么」在校验方毫不知情的情况下扩展；
// 少一个 iss 则意味着签发方变了却仍被接受。两者都必须 401，且签名是有效的——
// 证明拒绝来自键集校验本身，而不是别的环节顺带拦下的。
func TestMiddleware_claims键集严格_多键或少键都拒绝(t *testing.T) {
	cfg := Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)}

	extra := validPayload(fixedNow)
	extra["scope"] = "orders:write"

	missing := validPayload(fixedNow)
	delete(missing, "iss")

	// 键数仍为 5，但把 iss 换成了 issuer：只数「有几个键」的校验会放过它，
	// 只有真正比对「键集」才能拦下——这正是决定实现是否合格的一条。
	renamed := validPayload(fixedNow)
	delete(renamed, "iss")
	renamed["issuer"] = "crosspilot"

	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"多一个 scope", extra},
		{"少一个 iss", missing},
		{"把 iss 换成 issuer（键数不变）", renamed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok := signToken(t, testSecret, validHeader(), tc.payload)
			res := runMiddleware(cfg, true, "Bearer "+tok)
			if res.rec.Code != http.StatusUnauthorized {
				t.Fatalf("claims 键集不合规应 401，实际 %d", res.rec.Code)
			}
			if res.called {
				t.Error("claims 键集不合规时绝不能调用下游处理器")
			}
		})
	}
}

// TestMiddleware_strict_aud_数组形式拒绝 守住 strict_aud。
//
// RFC 7519 允许 aud 是数组；本契约要求单字符串，因为多受众会让
// 「这个令牌是发给谁的」失去唯一答案。实现若用宽松的 Unmarshal 到
// []string 或 any，这条会红。
func TestMiddleware_strict_aud_数组形式拒绝(t *testing.T) {
	payload := validPayload(fixedNow)
	payload["aud"] = []string{"crosspilot-api", "other-service"}
	tok := signToken(t, testSecret, validHeader(), payload)

	res := runMiddleware(Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)},
		true, "Bearer "+tok)

	if res.rec.Code != http.StatusUnauthorized {
		t.Fatalf("aud 为数组应 401（strict_aud），实际 %d", res.rec.Code)
	}
	if res.called {
		t.Error("aud 形态不合规时绝不能调用下游处理器")
	}
}

// TestMiddleware_过期令牌_拒绝 覆盖超出偏移窗口的过期。
//
// 过期判定被写反（例如条件用了 Before）时，所有过期令牌都会被放行，
// 而这类错误在手工冒烟里几乎不可能被发现。
func TestMiddleware_过期令牌_超出偏移窗口_拒绝(t *testing.T) {
	payload := validPayload(fixedNow)
	// 过期 61 秒：超过 1 分钟偏移窗口，必须拒绝。
	payload["exp"] = fixedNow.Add(-61 * time.Second).Unix()
	tok := signToken(t, testSecret, validHeader(), payload)

	res := runMiddleware(Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)},
		true, "Bearer "+tok)

	if res.rec.Code != http.StatusUnauthorized {
		t.Fatalf("已过期令牌应 401，实际 %d", res.rec.Code)
	}
	if res.called {
		t.Error("已过期令牌绝不能调用下游处理器")
	}
}

// TestMiddleware_时钟偏移窗口内_通过 覆盖 ±1 分钟的宽容边界。
//
// 这条与「过期拒绝」是一对：只有两者都在，才能证明实现不是简单地
// 「只要 exp < now 就拒」（那会让快了几十秒的客户端随机 401），
// 也不是「完全不校验 exp」。
func TestMiddleware_时钟偏移窗口内_通过(t *testing.T) {
	cases := []struct {
		name string
		exp  time.Time
	}{
		// 刚好过期 60 秒：exp + skew == now，按「After」语义不算过期，应放行。
		{"恰好偏移一分钟", fixedNow.Add(-maxClockSkew)},
		// 过期 30 秒：常见的 NTP 漂移，属于窗口内。
		{"偏移半分钟", fixedNow.Add(-30 * time.Second)},
		// 未过期：正常令牌。
		{"尚未过期", fixedNow.Add(time.Minute)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := validPayload(fixedNow)
			payload["exp"] = tc.exp.Unix()
			tok := signToken(t, testSecret, validHeader(), payload)
			res := runMiddleware(Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)},
				true, "Bearer "+tok)
			if res.rec.Code != http.StatusOK {
				t.Fatalf("偏移窗口内的令牌应放行，状态码 = %d，body=%s", res.rec.Code, res.rec.Body.String())
			}
			if !res.called {
				t.Error("窗口内的令牌必须调用下游处理器")
			}
		})
	}
}

// TestMiddleware_签发时间在未来_超出窗口拒绝 覆盖 iat 的前向偏移。
//
// 未来 nbf 的缺失在这里由 iat 承担：一个「签发于 2 分钟后」的令牌要么是
// 时钟错乱，要么是被伪造，超出窗口就该拒绝。
func TestMiddleware_签发时间在未来_超出窗口拒绝(t *testing.T) {
	payload := validPayload(fixedNow)
	payload["iat"] = fixedNow.Add(61 * time.Second).Unix()
	tok := signToken(t, testSecret, validHeader(), payload)

	res := runMiddleware(Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)},
		true, "Bearer "+tok)

	if res.rec.Code != http.StatusUnauthorized {
		t.Fatalf("签发时间超出未来窗口应 401，实际 %d", res.rec.Code)
	}
	if res.called {
		t.Error("签发时间异常的令牌绝不能调用下游处理器")
	}
}

// TestMiddleware_Authorization头形态_拒绝 覆盖取令牌这一步的所有畸形输入。
//
// 每一种形态都对应一个真实的客户端 bug 或攻击载荷；这些分支如果被写成
// 「宽松地截取最后一段」，缺 token 的头会被当成空串、多段头会被截断——
// 两者都会把一个本该 401 的请求送进校验逻辑。
func TestMiddleware_Authorization头形态_拒绝(t *testing.T) {
	cfg := Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)}

	valid := signToken(t, testSecret, validHeader(), validPayload(fixedNow))
	cases := []struct {
		name     string
		set      bool
		rawValue string
	}{
		{"完全缺少 Authorization 头", false, ""},
		{"不是 Bearer 前缀", true, "Token " + valid},
		{"只有 Bearer 没有 token", true, "Bearer"},
		{"Bearer 后只有空白", true, "Bearer   "},
		{"只有两段", true, "Bearer aaa.bbb"},
		{"有四段", true, "Bearer aaa.bbb.ccc.ddd"},
		{"单段无点", true, "Bearer aaa"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runMiddleware(cfg, tc.set, tc.rawValue)
			if res.rec.Code != http.StatusUnauthorized {
				t.Fatalf("畸形 Authorization 头应 401，实际 %d，body=%s", res.rec.Code, res.rec.Body.String())
			}
			if res.called {
				t.Error("畸形 Authorization 头绝不能调用下游处理器")
			}
		})
	}
}

// TestMiddleware_空密钥_放行且不注入身份 覆盖「本部署未启用鉴权」的语义。
//
// Secret 为空代表这一层没启用，决策权留给部署者——但「未启用」不等于
// 「用空密钥校验」：必须完全不解析令牌，也不能凭空造出一个买家身份，
// 否则所有请求都会以同一个空身份落库。
func TestMiddleware_空密钥_放行且不注入身份(t *testing.T) {
	// 故意带一个畸形令牌：空密钥时它根本不该被看一眼。
	res := runMiddleware(Config{Secret: nil, Typ: testTyp, Clock: fixedClock(fixedNow)},
		true, "Bearer not-a-token")

	if res.rec.Code != http.StatusOK {
		t.Fatalf("未配置密钥时应无条件放行，状态码 = %d", res.rec.Code)
	}
	if !res.called {
		t.Fatal("未配置密钥时必须调用下游处理器")
	}
	if res.buyerID != "" {
		t.Errorf("未配置密钥时不应凭空注入身份，实际 BuyerFrom(ctx) = %q", res.buyerID)
	}
}

// --- identity.go ---------------------------------------------------------

// runIdentityMiddleware 用给定 context 执行一层身份中间件，返回状态码、
// 下游是否执行、以及下游看到的买家/会话。
func runIdentityMiddleware(
	ctx context.Context, mw func(http.Handler) http.Handler,
) (int, bool, string, string) {
	called := false
	buyer, session := "", ""
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		buyer, session = BuyerFrom(r.Context()), SessionFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)
	return rec.Code, called, buyer, session
}

// TestSessionHeader_读取会话头 守住会话来源。
//
// 会话刻意不放进 JWT claim（claim 键集被钉死），只能走 X-Session-ID 头。
// 若实现改成读别的头名，所有多标签页场景会共用一个会话，
// 而单测里「读到了会话」这类正向断言最容易把这个改动放过。
func TestSessionHeader_读取会话头(t *testing.T) {
	ctx := context.Background()

	// 带了会话头：必须被读出来。
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(headerSessionID, "sess-abc")
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := SessionFrom(r.Context()); got != "sess-abc" {
			t.Errorf("SessionFrom(ctx) = %q，期望 sess-abc", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	SessionHeader()(next).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("SessionHeader 不应拦截请求，状态码 = %d", rec.Code)
	}

	// 没带会话头：不应凭空造一个，且请求仍应放行（是否需要会话由端点决定）。
	code, called, _, session := runIdentityMiddleware(ctx, SessionHeader())
	if code != http.StatusOK || !called {
		t.Fatalf("缺少会话头时 SessionHeader 仍应放行，code=%d called=%v", code, called)
	}
	if session != "" {
		t.Errorf("缺少会话头时 SessionFrom 应为空，实际 %q", session)
	}
}

// TestDemoIdentity_不覆盖已有身份 是本次重构最该守住的一条。
//
// 配错或尚未启用密钥的环境会挂上 DemoIdentity 兜底。如果它无条件覆盖，
// 所有调用者都会被改写成同一个 demo 买家——越权与串号会真实发生，
// 而日志里一点异常都看不出来。会话同理：已有会话不能被 fallback 顶掉。
func TestDemoIdentity_不覆盖已有身份(t *testing.T) {
	mw := DemoIdentity("demo-buyer", "demo-session")

	// 已有真实身份：必须原样保留。
	ctx := WithSession(WithBuyer(context.Background(), "real-buyer"), "real-session")
	code, called, buyer, session := runIdentityMiddleware(ctx, mw)
	if !called {
		t.Fatal("DemoIdentity 必须放行请求")
	}
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d", code)
	}
	if buyer != "real-buyer" {
		t.Errorf("已有身份被覆盖：BuyerFrom = %q，期望保留 real-buyer", buyer)
	}
	if session != "real-session" {
		t.Errorf("已有会话被覆盖：SessionFrom = %q，期望保留 real-session", session)
	}

	// 没有身份：补上 demo 身份与会话。
	_, _, buyer, session = runIdentityMiddleware(context.Background(), mw)
	if buyer != "demo-buyer" {
		t.Errorf("无身份时应补 demo 买家，实际 %q", buyer)
	}
	if session != "demo-session" {
		t.Errorf("无会话时应补 fallback 会话，实际 %q", session)
	}
}

// TestRequireBuyer_无身份时拒绝 守住「必须买家身份」端点。
//
// 若这层被实现成「总是放行」，那么一条漏挂 JWT 中间件的路由会直接
// 以空买家身份访问下游，把别人的数据当成自己的。
func TestRequireBuyer_无身份时拒绝(t *testing.T) {
	mw := RequireBuyer(nil)

	// 无身份：401，且下游不得执行。
	code, called, _, _ := runIdentityMiddleware(context.Background(), mw)
	if code != http.StatusUnauthorized {
		t.Fatalf("无买家身份应 401，实际 %d", code)
	}
	if called {
		t.Error("无买家身份时绝不能调用下游处理器")
	}

	// 有身份：放行。
	ctx := WithBuyer(context.Background(), "buyer-1")
	code, called, buyer, _ := runIdentityMiddleware(ctx, mw)
	if code != http.StatusOK || !called {
		t.Fatalf("有身份应放行，code=%d called=%v", code, called)
	}
	if buyer != "buyer-1" {
		t.Errorf("下游看到的买家 = %q，期望 buyer-1", buyer)
	}
}

// TestRequireSession_无会话时400 守住会话缺失的语义。
//
// 缺会话返回 400 而不是 401：调用方身份没问题，缺的是这次请求的参数。
// 用 401 会让客户端误以为该刷新令牌，而重试同一个请求永远不会成功——
// 这条把状态码钉死，正是防止实现「顺手复用 reject 的 401」。
func TestRequireSession_无会话时400(t *testing.T) {
	mw := RequireSession(nil)

	// 无会话：400，且下游不得执行。
	code, called, _, _ := runIdentityMiddleware(context.Background(), mw)
	if code != http.StatusBadRequest {
		t.Fatalf("缺会话应 400，实际 %d", code)
	}
	if called {
		t.Error("缺会话时绝不能调用下游处理器")
	}

	// 有会话：放行。
	ctx := WithSession(context.Background(), "sess-1")
	code, called, _, session := runIdentityMiddleware(ctx, mw)
	if code != http.StatusOK || !called {
		t.Fatalf("有会话应放行，code=%d called=%v", code, called)
	}
	if session != "sess-1" {
		t.Errorf("下游看到的会话 = %q，期望 sess-1", session)
	}
}

// TestMiddleware_配置了logger_失败仍返回401 覆盖 reject 的日志分支。
//
// 记日志与拒绝是两件事：配了 logger 时失败原因进日志，但对外的响应
// 必须仍是统一的 401，绝不能把内部原因（签名不对 / claim 过期）泄漏给
// 调用方——那等于给令牌探测提供了反馈信道。
func TestMiddleware_配置了logger_失败仍返回401(t *testing.T) {
	cfg := Config{
		Secret: testSecret,
		Typ:    testTyp,
		Clock:  fixedClock(fixedNow),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	// 缺 Authorization 头，必然走 reject 且 logger 非空。
	res := runMiddleware(cfg, false, "")
	if res.rec.Code != http.StatusUnauthorized {
		t.Fatalf("状态码 = %d，期望 401", res.rec.Code)
	}
	if strings.Contains(res.rec.Body.String(), "签名") || strings.Contains(res.rec.Body.String(), "过期") {
		t.Errorf("响应不得泄漏失败原因，实际 %s", res.rec.Body.String())
	}
}

// TestMiddleware_typ不匹配_拒绝 覆盖 header.typ 的精确匹配。
//
// typ 是契约的一部分：配成别的期望值再收到本服务签发的令牌，必须拒绝。
// 若实现漏判 typ，跨系统的令牌（比如另一套用途不同的 HS256 令牌）只要
// 密钥碰巧相同就会被接受。
func TestMiddleware_typ不匹配_拒绝(t *testing.T) {
	tok := signToken(t, testSecret, validHeader(), validPayload(fixedNow))
	// 期望值是 OTHER，而令牌的 typ 是 JWT。
	res := runMiddleware(Config{Secret: testSecret, Typ: "OTHER", Clock: fixedClock(fixedNow)},
		true, "Bearer "+tok)

	if res.rec.Code != http.StatusUnauthorized {
		t.Fatalf("typ 不匹配应 401，实际 %d", res.rec.Code)
	}
	if res.called {
		t.Error("typ 不匹配时绝不能调用下游处理器")
	}
}

// TestMiddleware_header畸形_拒绝 覆盖 header 解码与取字段的失败分支。
//
// 这些输入都是「形状就不对」的令牌：header 不是 JSON 对象、alg 不是字符串。
// 它们在验签之前就该被挡住，且不能因为 base64 合法就继续往下读 claims。
func TestMiddleware_header畸形_拒绝(t *testing.T) {
	payload := b64(mustJSON(t, validPayload(fixedNow)))
	cfg := Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)}

	cases := []struct {
		name      string
		headerSeg string
	}{
		// 合法 base64，但解出来是数组而不是对象。
		{"header 是数组而非对象", b64([]byte("[]"))},
		// 合法 base64，但完全是纯文本。
		{"header 是纯文本", b64([]byte("not json"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runMiddleware(cfg, true, "Bearer "+tc.headerSeg+"."+payload+"."+b64([]byte("sig")))
			if res.rec.Code != http.StatusUnauthorized {
				t.Fatalf("畸形 header 应 401，实际 %d", res.rec.Code)
			}
			if res.called {
				t.Error("畸形 header 绝不能调用下游处理器")
			}
		})
	}

	// alg 不是字符串：signToken 能正常构造，但字段类型违约。
	t.Run("alg 是数字", func(t *testing.T) {
		bad := signToken(t, testSecret, map[string]any{"alg": 1, "typ": testTyp}, validPayload(fixedNow))
		res := runMiddleware(cfg, true, "Bearer "+bad)
		if res.rec.Code != http.StatusUnauthorized {
			t.Fatalf("alg 非字符串应 401，实际 %d", res.rec.Code)
		}
		if res.called {
			t.Error("alg 非字符串绝不能调用下游处理器")
		}
	})
}

// TestMiddleware_令牌段不是合法base64_拒绝 覆盖 decodeSegment 的失败分支。
//
// 旁路在 base64 解码阶段就该被拦住；若实现把解码错误当成空串继续，
// 后面所有的形状校验都会作用在错误的输入上。
func TestMiddleware_令牌段不是合法base64_拒绝(t *testing.T) {
	cfg := Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)}
	payload := b64(mustJSON(t, validPayload(fixedNow)))
	header := b64(mustJSON(t, validHeader()))

	cases := []struct {
		name string
		tok  string
	}{
		{"header 段非法 base64", "!!!." + payload + "." + b64([]byte("sig"))},
		{"payload 段非法 base64", header + ".!!!." + b64([]byte("sig"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runMiddleware(cfg, true, "Bearer "+tc.tok)
			if res.rec.Code != http.StatusUnauthorized {
				t.Fatalf("非法 base64 应 401，实际 %d", res.rec.Code)
			}
			if res.called {
				t.Error("非法 base64 绝不能调用下游处理器")
			}
		})
	}
}

// TestMiddleware_claims取值类型错误_拒绝 覆盖取具体字段时的类型与空值校验。
//
// 键集对了不代表值对：iat/exp 必须是整数时间戳，sub/iss/aud 必须是字符串，
// sub 还不能为空。这些值一旦被宽松地「零值充数」，时间校验会全部失效。
func TestMiddleware_claims取值类型错误_拒绝(t *testing.T) {
	cfg := Config{Secret: testSecret, Typ: testTyp, Clock: fixedClock(fixedNow)}

	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"iat 是字符串", func(m map[string]any) { m["iat"] = "1700000000" }},
		{"exp 是字符串", func(m map[string]any) { m["exp"] = "1700000000" }},
		{"sub 是数字", func(m map[string]any) { m["sub"] = 123 }},
		{"sub 为空串", func(m map[string]any) { m["sub"] = "" }},
		{"iss 是数字", func(m map[string]any) { m["iss"] = 5 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := validPayload(fixedNow)
			tc.mutate(payload)
			tok := signToken(t, testSecret, validHeader(), payload)
			res := runMiddleware(cfg, true, "Bearer "+tok)
			if res.rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s 应 401，实际 %d", tc.name, res.rec.Code)
			}
			if res.called {
				t.Error("claims 取值非法时绝不能调用下游处理器")
			}
		})
	}
}

// TestDecode字段_缺少键时返回错误 覆盖两个取值辅助的「键不存在」分支。
//
// 在 verify 的调用路径上，键集校验保证了这两个分支不会被触发；
// 但它们存在的意义是：任何绕过键集校验的新调用点都能得到明确错误，
// 而不是静默产出零值。这是防御性代码，用直接调用来钉住它的行为。
func TestDecode字段_缺少键时返回错误(t *testing.T) {
	empty := map[string]json.RawMessage{}

	var s string
	if err := decodeStringField(empty, "sub", &s); err == nil {
		t.Error("缺少字符串 claim 应返回错误，而不是留下空串")
	}
	var n int64
	if err := decodeInt64Field(empty, "exp", &n); err == nil {
		t.Error("缺少整数 claim 应返回错误，而不是留下零值")
	}
}

// mustJSON 序列化 map，失败即失败测试。
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	return b
}
