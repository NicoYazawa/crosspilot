// ConfirmationCard 组件：订单确认完成卡。

import type { ComponentProps } from '../registry';

export function ConfirmationCard({ id, props }: ComponentProps) {
  const title = typeof props?.['title'] === 'string' ? (props['title'] as string) : '已确认';
  const orderId = typeof props?.['order_id'] === 'string' ? (props['order_id'] as string) : '';
  return (
    <section className="a2ui-confirmation-card" data-component-id={id}>
      <h3>{title}</h3>
      {orderId ? <p>订单号：{orderId}</p> : null}
    </section>
  );
}
