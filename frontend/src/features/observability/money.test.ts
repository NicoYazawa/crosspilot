// money.ts — 最小货币单位换算。
//
// 这一层防的是「后端给的是 1e-6 元为单位的整数，前端当成元裸打印」——
// 差六个数量级，且显示出来仍然是一个像模像样的数字，看不出错。

import { describe, expect, it } from 'vitest';
import { MINOR_PER_UNIT, formatCost, formatMinor } from './money';

describe('formatMinor', () => {
  it('1e6 最小单位 = 1 个货币单位', () => {
    expect(MINOR_PER_UNIT).toBe(1_000_000);
    expect(formatMinor(1_000_000)).toBe('1');
    expect(formatMinor(2_500_000)).toBe('2.5');
  });

  it('一次真实下单查询的量级（13600 = 0.0136 元）', () => {
    // 实测量级：3600 input + 800 output 的 deepseek-flash 调用。
    // 若最小单位是「分」，这个数字会是 0——正是本次改口径要解决的问题。
    expect(formatMinor(13_600)).toBe('0.0136');
  });

  it('0 显示为 0（确实没有产生费用）', () => {
    expect(formatMinor(0)).toBe('0');
  });

  it('非 0 但小于一个最小单位不显示成 0', () => {
    // 把「有费用但小到显示不出来」渲染成 0，与「没花钱」就分不出来了。
    expect(formatMinor(0.4)).toBe('<0.000001');
    expect(formatMinor(-0.4)).toBe('>-0.000001');
  });

  it('去掉尾随 0', () => {
    expect(formatMinor(1)).toBe('0.000001');
    expect(formatMinor(100)).toBe('0.0001');
    expect(formatMinor(10_000)).toBe('0.01');
    expect(formatMinor(1_230_000)).toBe('1.23');
  });

  it('大额也按 6 位小数收敛', () => {
    expect(formatMinor(1_234_567_890)).toBe('1234.56789');
  });

  it('非有限数显示占位符而不是 NaN', () => {
    expect(formatMinor(Number.NaN)).toBe('—');
    expect(formatMinor(Number.POSITIVE_INFINITY)).toBe('—');
  });
});

describe('formatCost', () => {
  it('带币种', () => {
    expect(formatCost(13_600, 'CNY')).toBe('0.0136 CNY');
    expect(formatCost(2_000_000, 'USD')).toBe('2 USD');
  });

  it('币种为空时不留下多余空格', () => {
    expect(formatCost(13_600, '')).toBe('0.0136');
  });
});
