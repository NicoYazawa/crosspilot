// 鉴权头构造测试。
//
// 覆盖：token 转 Bearer 形态、session 头、以及「两者都缺」时不注入任何头——
// 空 token 若被渲染成 "Bearer " 会让后端按「令牌畸形」401，而正确语义是
// 「没带令牌」。

import { describe, expect, it } from 'vitest';
import { buildAuthHeaders } from './auth';

describe('buildAuthHeaders', () => {
  it('token 渲染为 Bearer 形态', () => {
    const headers = buildAuthHeaders('abc.def.ghi');
    expect(headers['Authorization']).toBe('Bearer abc.def.ghi');
  });

  it('session 走独立请求头，不进 Authorization', () => {
    const headers = buildAuthHeaders(undefined, 'sess-1');
    expect(headers['X-Session-ID']).toBe('sess-1');
    expect(headers['Authorization']).toBeUndefined();
  });

  it('空 token 不注入 Authorization（避免 "Bearer " 畸形头）', () => {
    const headers = buildAuthHeaders('', 'sess-1');
    expect(headers['Authorization']).toBeUndefined();
    expect(Object.keys(headers)).toEqual(['X-Session-ID']);
  });

  it('两者都缺时返回空对象', () => {
    expect(buildAuthHeaders()).toEqual({});
  });
});
