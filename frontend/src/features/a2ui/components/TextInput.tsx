// TextInput 组件：单行文本输入。
//
// 仅展示态：value 来自 props.value，提交由父表单的 onSubmit 处理。
// 不在此组件内置状态——避免与 useOptimistic 冲突。

import type { ComponentProps } from '../registry';

export function TextInput({ id, props }: ComponentProps) {
  const label = typeof props?.['label'] === 'string' ? (props['label'] as string) : '';
  const placeholder = typeof props?.['placeholder'] === 'string' ? (props['placeholder'] as string) : '';
  const value = typeof props?.['value'] === 'string' ? (props['value'] as string) : '';
  return (
    <label className="a2ui-text-input" data-component-id={id}>
      {label ? <span>{label}</span> : null}
      <input type="text" defaultValue={value} placeholder={placeholder} />
    </label>
  );
}
