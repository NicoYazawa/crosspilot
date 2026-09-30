// ApprovalCard 组件：审批卡（标题 + 描述 + confirmation_id）。

import type { ComponentProps } from '../registry';

export function ApprovalCard({ id, props }: ComponentProps) {
  const title = typeof props?.['title'] === 'string' ? (props['title'] as string) : '是否确认';
  const description = typeof props?.['description'] === 'string' ? (props['description'] as string) : '';
  const confirmationId = typeof props?.['confirmation_id'] === 'string' ? (props['confirmation_id'] as string) : '';
  return (
    <section className="a2ui-approval-card" data-component-id={id}>
      <h3>{title}</h3>
      {description ? <p>{description}</p> : null}
      <p className="muted">confirmation_id: {confirmationId}</p>
    </section>
  );
}
