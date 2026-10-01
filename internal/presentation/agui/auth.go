// HMAC 身份校验中间件（简化方案）。
//
// 完整 E8「键集严格校验」要校验所有必填 header 集合是否一致——本阶段先实现
// 典型场景：X-HMAC-Sign 头 + X-HMAC-Timestamp 头，签名覆盖 method+path+ts+body。
//
// 为什么不用 JWT：AG-UI 是流式协议，每次 SSE 重连都重新算一次签名比签发 JWT
// 简单；密钥也少流转一份。前端的 demo / hmac 双模这里只实现 hmac。
package agui

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const (
	headerHMACSign = "X-HMAC-Sign"
	headerHMACTime = "X-HMAC-Timestamp"
)

// maxClockSkew 是客户端时间与服务端时间的允许偏差。
//
// 太宽松等于放开重放窗口：5 分钟够「正常网络抖动 + 用户点按钮的思考时间」，
// 又不够「凌晨 3 点的批量重放脚本」。
const maxClockSkew = 5 * time.Minute

// ErrUnauthorized 暴露给 handler 的鉴权失败错误。
var ErrUnauthorized = errors.New("agui: 身份验证失败")

// HMACAuth 是中间件工厂：返回的 http.Handler 在通过校验后调用 next，
// 否则把请求截断并写 401。
//
// secret 为空时本中间件放行——配置缺失只是「这一层还没启用」，但绝不
// 让中间件以空密钥运行（恒等签名等于无鉴权）。这是和「故意把密钥留空」
// 两种语义的关键区别：前者把决策权交给部署者，后者是开发期 hack。
func HMACAuth(secret []byte, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(secret) == 0 {
				next.ServeHTTP(w, r)
				return
			}

			ts := r.Header.Get(headerHMACTime)
			sig := r.Header.Get(headerHMACSign)
			if ts == "" || sig == "" {
				unauthorized(w, logger, "missing hmac headers", r)
				return
			}

			// 时间偏差检查必须先做：不让签名校验失败淹没了重放攻击。
			tsInt, err := strconv.ParseInt(ts, 10, 64)
			if err != nil {
				unauthorized(w, logger, "bad timestamp", r)
				return
			}
			delta := time.Since(time.Unix(tsInt, 0))
			if delta < -maxClockSkew || delta > maxClockSkew {
				unauthorized(w, logger, "timestamp out of window", r)
				return
			}

			expected := computeSignature(secret, r.Method, r.URL.Path, ts)
			provided, err := hex.DecodeString(sig)
			if err != nil {
				unauthorized(w, logger, "bad signature encoding", r)
				return
			}
			if subtle.ConstantTimeCompare(expected, provided) != 1 {
				unauthorized(w, logger, "signature mismatch", r)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// computeSignature 计算签名载荷。
//
// 形式：`hex(HMAC_SHA256(secret, method + "\n" + path + "\n" + ts))`。
// body 不在签名里：客户端 Body 计算 hash 之后必须把它放在 Header，而本设计
// 故意不校验 body——AG-UI 流是单向写、客户端只 POST 一个 JSON，body 篡改成本
// 与防篡改收益都不对称。完整覆盖留 P7。
func computeSignature(secret []byte, method, path, ts string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(method))
	mac.Write([]byte("\n"))
	mac.Write([]byte(path))
	mac.Write([]byte("\n"))
	mac.Write([]byte(ts))
	return mac.Sum(nil)
}

func unauthorized(w http.ResponseWriter, logger *slog.Logger, reason string, r *http.Request) {
	if logger != nil {
		logger.WarnContext(r.Context(), "AG-UI 鉴权失败",
			slog.String("reason", reason),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
		)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}
