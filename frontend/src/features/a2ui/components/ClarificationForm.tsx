// ClarificationForm 组件：澄清表单（问题 + 选项列表）。

import type { ComponentProps } from '../registry';

export function ClarificationForm({ id, props }: ComponentProps) {
  const question = typeof props?.['question'] === 'string' ? (props['question'] as string) : '';
  const options = (props?.['options'] as Array<string | Record<string, unknown>> | undefined) ?? [];
  return (
    <section className="a2ui-clarification-form" data-component-id={id}>
      <p>{question}</p>
      <ul>
        {options.map((opt, i) => {
          const label = typeof opt === 'string' ? opt : typeof (opt as Record<string, unknown>)['label'] === 'string' ? ((opt as Record<string, unknown>)['label'] as string) : '';
          return <li key={i}>{label}</li>;
        })}
      </ul>
    </section>
  );
}
