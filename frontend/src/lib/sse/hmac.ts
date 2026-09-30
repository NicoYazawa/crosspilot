// HMAC 签名工具（前端 SubtleCrypto 版本）。
//
// 与后端 internal/presentation/agui/auth.go:91-99 对齐：
//   - 签名载荷：hex(HMAC_SHA256(secret, method + "\n" + path + "\n" + ts))
//   - 时间偏差窗口：5 分钟（maxClockSkew）
//   - 请求头：X-HMAC-Sign + X-HMAC-Timestamp
//
// body 不在签名内（与后端一致）：AG-UI 是单向写、客户端只 POST 一个 JSON，
// body 篡改成本与防篡改收益都不对称。完整覆盖留 P7。

const HEADER_SIGN = 'X-HMAC-Sign';
const HEADER_TIME = 'X-HMAC-Timestamp';

const MAX_CLOCK_SKEW_MS = 5 * 60 * 1000;

export interface HmacHeaders {
  [HEADER_SIGN]: string;
  [HEADER_TIME]: string;
}

export interface SignArgs {
  method: string;
  path: string;
  secret: string;
  now?: number;
}

// 构造带 HMAC 头的 Record；secret 为空时返回空对象。
//
// 返回对象用 Object.freeze 防止调用方误改（特别是 Date.now 测试期）。
export async function buildHmacHeaders(args: SignArgs): Promise<Record<string, string>> {
  const { method, path, secret } = args;
  if (secret === '') return {};
  const ts = String(args.now ?? Date.now());
  const sign = await computeSign(secret, method, path, ts);
  return Object.freeze({ [HEADER_SIGN]: sign, [HEADER_TIME]: ts });
}

// 直接计算签名（hex 字符串）。暴露给测试用——生产里走 buildHmacHeaders。
export async function computeSign(
  secret: string,
  method: string,
  path: string,
  ts: string,
): Promise<string> {
  const enc = new TextEncoder();
  const key = await crypto.subtle.importKey(
    'raw',
    enc.encode(secret),
    { name: 'HMAC', hash: 'SHA-256' },
    false,
    ['sign'],
  );
  const data = enc.encode(`${method}\n${path}\n${ts}`);
  const sig = await crypto.subtle.sign('HMAC', key, data);
  return toHex(new Uint8Array(sig));
}

function toHex(bytes: Uint8Array): string {
  let out = '';
  for (const b of bytes) out += b.toString(16).padStart(2, '0');
  return out;
}

// 校验客户端时间是否在 ±5 分钟窗口内。
//
// 主要给 SSE 重连 / 轮询请求用——服务端有自己的校验，前端预校验可以避免
// 「明明时钟漂了还在重试」的循环。
export function isWithinClockSkew(clientTs: number, serverNow: number = Date.now()): boolean {
  const delta = Math.abs(serverNow - clientTs);
  return delta <= MAX_CLOCK_SKEW_MS;
}
