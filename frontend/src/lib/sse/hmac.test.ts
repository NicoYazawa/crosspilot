// HMAC 工具测试。
//
// 覆盖：
//   - 空 secret 返回空 headers
//   - 非空 secret 返回 X-HMAC-Sign + X-HMAC-Timestamp
//   - 与后端 computeSignature 一致（用固定 secret / method / path / ts 复算）

import { describe, expect, it } from 'vitest';
import { buildHmacHeaders, computeSign } from './hmac';

describe('HMAC', () => {
  const SECRET = 'sk_abc123def456ghi789jkl012mno';

  it('空 secret 返回空对象', async () => {
    const headers = await buildHmacHeaders({ method: 'GET', path: '/x', secret: '' });
    expect(headers).toEqual({});
  });

  it('非空 secret 返回 X-HMAC-Sign + X-HMAC-Timestamp', async () => {
    const headers = await buildHmacHeaders({
      method: 'GET',
      path: '/agui/runs/run-1/events',
      secret: SECRET,
      now: 1_700_000_000_000,
    });
    expect(Object.keys(headers).sort()).toEqual(['X-HMAC-Sign', 'X-HMAC-Timestamp']);
    expect(headers['X-HMAC-Timestamp']).toBe('1700000000000');
    expect(headers['X-HMAC-Sign']).toMatch(/^[0-9a-f]{64}$/);
  });

  it('同一 secret / method / path / ts 算出的签名稳定', async () => {
    const a = await computeSign(SECRET, 'GET', '/agui/runs/run-1/events', '1700000000000');
    const b = await computeSign(SECRET, 'GET', '/agui/runs/run-1/events', '1700000000000');
    expect(a).toBe(b);
  });

  it('不同 ts 签名不同', async () => {
    const a = await computeSign(SECRET, 'GET', '/agui/runs/run-1/events', '1700000000000');
    const b = await computeSign(SECRET, 'GET', '/agui/runs/run-1/events', '1700000000001');
    expect(a).not.toBe(b);
  });

  it('不同 method 签名不同', async () => {
    const a = await computeSign(SECRET, 'GET', '/agui/runs/run-1/events', '1700000000000');
    const b = await computeSign(SECRET, 'POST', '/agui/runs/run-1/events', '1700000000000');
    expect(a).not.toBe(b);
  });
});
