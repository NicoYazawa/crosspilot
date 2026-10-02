// 金额换算：后端的最小货币单位 → 人能读的货币单位。
//
// cost_minor 的单位是 **1e-6 元**（见 migrations/0005_cost_unit_micro），
// 不是「分」。列名与字段名保持不变，变的只是最小单位是什么。
//
// 为什么不是分：一次真实的下单查询总成本只有零点几分，按分取整后每一行都是 0，
// 成本面板会长期显示「没花钱」——那正是 F4 要防的那类静默失败。

// MINOR_PER_UNIT 是 1 个货币单位对应多少最小单位（1 元 = 1e6）。
export const MINOR_PER_UNIT = 1_000_000;

// 能显示的最小值：一个最小单位。
const ONE_MINOR = 1 / MINOR_PER_UNIT;

/**
 * formatMinor 把 cost_minor 渲染成可读的金额字符串（不含币种）。
 *
 * 三条规则：
 *   - 0 → "0"：确实没有产生费用（已定价且用量为 0）。
 *   - 非 0 但小于一个最小单位 → "<0.000001"：绝不显示成 "0"。把「有费用但小到
 *     看不见」渲染成 0，与「没花钱」在面板上就分不出来了。
 *   - 其余 → 最多 6 位小数并去掉尾随 0。
 */
export function formatMinor(minor: number): string {
  if (!Number.isFinite(minor)) return '—';
  if (minor === 0) return '0';

  const units = minor / MINOR_PER_UNIT;
  if (Math.abs(units) < ONE_MINOR) {
    return minor > 0 ? '<0.000001' : '>-0.000001';
  }
  // toFixed(6) 之后去掉尾随 0 与孤立的小数点：0.013600 → 0.0136，1.000000 → 1。
  return units.toFixed(6).replace(/\.?0+$/, '');
}

/**
 * formatCost 是 formatMinor 加上币种后缀的便捷形式。
 *
 * 币种随每行数据给出：不同 provider 可以不同（Anthropic 按美元报价），
 * 所以不能在整个面板上用一个全局币种。
 */
export function formatCost(minor: number, currency: string): string {
  const amount = formatMinor(minor);
  return currency === '' ? amount : `${amount} ${currency}`;
}
