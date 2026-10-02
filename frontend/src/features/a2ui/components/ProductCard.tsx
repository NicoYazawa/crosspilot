// ProductCard 组件：单个商品卡片。
//
// props 的键名与后端 A2UI emitter 发出的完全一致（price_major 而不是
// price_minor）：orderflow/a2ui.go 与 orderflow/a2ui_search.go 两份 emitter
// 都发 price_major，来源是 catalogsearch.ProductCard.PriceMajor。price_minor
// 是交易账本侧的词（unit_price_minor，分），A2UI 这一层没有生产者——
// 之前这里读 price_minor，结果是每张卡片都渲染不出价格，而两侧各自的测试
// 都用自己的键名，谁也没发现。
//
// surface 是 dynamic，props 全部来自服务端 JSON：一律以 unknown 取值再收窄，
// 不做任何直接求值（G4 XSS 验收）。

import type { ComponentProps } from '../registry';

function str(props: Record<string, unknown> | undefined, key: string): string {
  const v = props?.[key];
  return typeof v === 'string' ? v : '';
}

function num(props: Record<string, unknown> | undefined, key: string): number | null {
  const v = props?.[key];
  return typeof v === 'number' && Number.isFinite(v) ? v : null;
}

export function ProductCard({ id, props }: ComponentProps) {
  const title = str(props, 'title');
  const subtitle = str(props, 'subtitle');
  const priceMajor = num(props, 'price_major');
  const currency = str(props, 'currency') || 'CNY';
  const productID = str(props, 'product_id');
  const skuID = str(props, 'sku_id');

  // 副标题：服务端显式给了 subtitle 就用它，否则用品牌 / 类目拼一条。
  // 都没有时不占位——空行会让卡片看起来坏了。
  const meta = subtitle || [str(props, 'brand'), str(props, 'category')].filter(Boolean).join(' · ');

  return (
    <article
      className="a2ui-product-card"
      data-component-id={id}
      data-product-id={productID || undefined}
      data-sku-id={skuID || undefined}
    >
      <h3>{title}</h3>
      {meta ? <p className="muted">{meta}</p> : null}
      {priceMajor !== null ? (
        <p>
          <strong>{priceMajor}</strong> {currency}
        </p>
      ) : null}
    </article>
  );
}
