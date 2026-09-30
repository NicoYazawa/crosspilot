// Text 组件：纯文本段落。
//
// variant: 'body' | 'h1' | 'h2' | 'muted'，默认 body。
// content / text 任一字段都行。

import type { ComponentProps } from '../registry';

export function Text({ id, props }: ComponentProps) {
  const content = asText(props?.['content'] ?? props?.['text']);
  const variant = asText(props?.['variant']) || 'body';
  const Tag: 'p' | 'h1' | 'h2' | 'span' = variant === 'h1' ? 'h1' : variant === 'h2' ? 'h2' : variant === 'muted' ? 'span' : 'p';
  const className = `a2ui-text a2ui-text-${variant}`;
  return (
    <Tag className={className} data-component-id={id}>
      {content}
    </Tag>
  );
}

function asText(v: unknown): string {
  if (typeof v === 'string') return v;
  if (v === null || v === undefined) return '';
  return String(v);
}
