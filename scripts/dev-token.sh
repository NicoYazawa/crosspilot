#!/usr/bin/env bash
#
# 本地开发用的 JWT 签发器：按附录 C#5 的契约生成一个 HS256 令牌，
# 用来在「已开启鉴权」的部署上手工打后端接口。
#
# 为什么需要它：开启鉴权后前端拿不到令牌（前端只能构建期注入，见
# docs/改动记录/2026-10-01/缺陷修复与契约对齐.md 的 3.3），但后端链路
# 仍然需要能验证——没有这个工具，验证 JWT 就只能靠手搓 Python。
#
# 用法：
#   ./scripts/dev-token.sh                          # 用默认买家，60 秒有效
#   ./scripts/dev-token.sh buyer-1 3600             # 指定买家与有效秒数
#   TOKEN=$(./scripts/dev-token.sh) curl -H "Authorization: Bearer $TOKEN" \
#       -H "X-Session-ID: s-1" localhost:8000/commerce/orders
#
# 只读 AUTH_JWT_SECRET，不打印它。令牌本身是凭据，别粘进 issue 或日志。

set -euo pipefail

buyer="${1:-dev-buyer}"
ttl="${2:-60}"
typ="${AUTH_JWT_TYP:-crosspilot-access+jwt}"

# 密钥来源：优先环境变量，其次仓库根的 .env。两者都没有就直接失败——
# 静默用一个空密钥签出来的令牌永远验不过，比报错更难查。
if [[ -z "${AUTH_JWT_SECRET:-}" && -f .env ]]; then
	AUTH_JWT_SECRET="$(sed -n 's/^AUTH_JWT_SECRET=//p' .env | head -1)"
fi
if [[ -z "${AUTH_JWT_SECRET:-}" ]]; then
	echo "dev-token: 未找到 AUTH_JWT_SECRET（环境变量与 .env 都没有）" >&2
	echo "           本部署可能未开启鉴权，那就不需要令牌。" >&2
	exit 1
fi

b64url() { openssl base64 -A | tr '+/' '-_' | tr -d '='; }

now="$(date +%s)"
exp="$((now + ttl))"

header="$(printf '{"alg":"HS256","typ":"%s"}' "$typ" | b64url)"
# 键集必须恰好是 {sub,iat,exp,iss,aud}：多一个键、少一个键都会被 401。
payload="$(printf '{"sub":"%s","iat":%s,"exp":%s,"iss":"crosspilot","aud":"crosspilot-api"}' \
	"$buyer" "$now" "$exp" | b64url)"

signing_input="${header}.${payload}"
sig="$(printf '%s' "$signing_input" | openssl dgst -sha256 -hmac "$AUTH_JWT_SECRET" -binary | b64url)"

printf '%s.%s\n' "$signing_input" "$sig"
