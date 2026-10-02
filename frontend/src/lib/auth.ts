// 鉴权头构造（Bearer JWT + 会话头）。
//
// 与后端 internal/presentation/auth/ 对齐：
//   - Authorization: Bearer <jwt>（jwt.go 校验 HS256 严格键集）
//   - X-Session-ID: <session>（identity.go，会话不进 JWT claim）
//
// 为什么前端不再自己签发：HMAC 方案要求把共享密钥打进浏览器包，任何人打开
// devtools 就能拿走，从而伪造任意买家身份。JWT 由签发方持有密钥，前端只负责
// 携带——把「能证明我是谁」的能力从前端挪走。
//
// token 为空时返回空对象，对应「本部署未启用鉴权」：装配层会补一个固定 demo
// 身份，开发与 E2E 照常跑通。

export const AUTHORIZATION_HEADER = 'Authorization';
export const SESSION_HEADER = 'X-Session-ID';

export interface AuthHeaders {
  [key: string]: string;
}

// buildAuthHeaders 构造鉴权相关的请求头。
//
// 两个参数都可选：只给 session 不给 token 是合法组合（未启用鉴权的部署靠
// X-Session-ID 区分会话），反过来同理。
export function buildAuthHeaders(token?: string, sessionId?: string): AuthHeaders {
  const headers: AuthHeaders = {};
  if (token) headers[AUTHORIZATION_HEADER] = `Bearer ${token}`;
  if (sessionId) headers[SESSION_HEADER] = sessionId;
  return headers;
}
