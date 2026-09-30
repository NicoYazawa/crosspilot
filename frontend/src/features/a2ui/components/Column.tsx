// Column 组件：垂直布局容器。
//
// props.children 是子组件 id 列表（由 Renderer 解析为 React.ReactNode）。

import type { ComponentProps } from '../registry';

export function Column({ id, children }: ComponentProps) {
  return (
    <div className="a2ui-column" data-component-id={id}>
      {children && Array.isArray(children) && children.length > 0 ? children : <span className="muted">(空 Column)</span>}
    </div>
  );
}
