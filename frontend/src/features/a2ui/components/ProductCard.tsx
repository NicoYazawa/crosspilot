// ProductCard 组件：单个商品卡片。

import type { ComponentProps } from '../registry';

export function ProductCard({ id, props }: ComponentProps) {
  const title = typeof props?.['title'] === 'string' ? (props['title'] as string) : '';
  const subtitle = typeof props?.['subtitle'] === 'string' ? (props['subtitle'] as string) : '';
  const priceMinor = typeof props?.['price_minor'] === 'number' ? (props['price_minor'] as number) : null;
  const currency = typeof props?.['currency'] === 'string' ? (props['currency'] as string) : 'CNY';
  return (
    <article className="a2ui-product-card" data-component-id={id}>
      <h3>{title}</h3>
      {subtitle ? <p className="muted">{subtitle}</p> : null}
      {priceMinor !== null ? (
        <p>
          <strong>{priceMinor}</strong> {currency}
        </p>
      ) : null}
    </article>
  );
}
