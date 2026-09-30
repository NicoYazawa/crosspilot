// List 组件：数组渲染。
//
// 优先 props.items；缺则用 props.data（updateDataModel 注入）。

import type { ComponentProps } from '../registry';

export function List({ id, props }: ComponentProps) {
  const items = (props?.['items'] as Array<Record<string, unknown>> | undefined) ?? [];
  const dataList = (props?.['data'] as Array<Record<string, unknown>> | undefined) ?? [];
  const renderItems = items.length > 0 ? items : dataList;
  return (
    <ul className="a2ui-list" data-component-id={id}>
      {renderItems.length === 0 ? <li className="muted">(空列表)</li> : null}
      {renderItems.map((item, i) => (
        <li key={i}>{renderListItem(item)}</li>
      ))}
    </ul>
  );
}

function renderListItem(item: unknown): React.ReactNode {
  if (typeof item === 'string') return item;
  if (typeof item !== 'object' || item === null) return JSON.stringify(item);
  const obj = item as Record<string, unknown>;
  const title = typeof obj['title'] === 'string' ? obj['title'] : typeof obj['name'] === 'string' ? obj['name'] : '';
  const subtitle = typeof obj['subtitle'] === 'string' ? obj['subtitle'] : typeof obj['description'] === 'string' ? obj['description'] : '';
  return (
    <span>
      <strong>{title}</strong>
      {subtitle ? <span className="muted"> — {subtitle}</span> : null}
    </span>
  );
}
