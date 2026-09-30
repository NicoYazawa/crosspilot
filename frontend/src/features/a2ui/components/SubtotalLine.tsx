// SubtotalLine 组件：小计行（label + amount）。

import type { ComponentProps } from '../registry';

export function SubtotalLine({ id, props }: ComponentProps) {
  const label = typeof props?.['label'] === 'string' ? (props['label'] as string) : '小计';
  const amountMinor = typeof props?.['amount_minor'] === 'number' ? (props['amount_minor'] as number) : 0;
  const currency = typeof props?.['currency'] === 'string' ? (props['currency'] as string) : 'CNY';
  return (
    <div className="a2ui-subtotal" data-component-id={id}>
      <span>{label}</span>
      <strong>
        {amountMinor} {currency}
      </strong>
    </div>
  );
}
