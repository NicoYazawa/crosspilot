// A2UI 组件注册表：type → React 组件。
//
// 增加新组件必须同步：
//   1. schema.ts 的 componentTypes 数组
//   2. types/a2ui.ts 的 A2UIComponentType 联合
//   3. components/<Name>.tsx 实现
//   4. 本 registry 注册
//
// 未注册的 type 在 schema 层就被拒（z.enum 闭字面量），本注册表运行期再
// 兜一次：万一未来有人扩了 schema 但忘了注册，渲染阶段会显式抛错而不是返回 undefined。

import type { ComponentType } from 'react';
import type { A2UIComponentType } from '@/types/a2ui';
import { Column } from './components/Column';
import { List } from './components/List';
import { Text } from './components/Text';
import { TextInput } from './components/TextInput';
import { ProductCard } from './components/ProductCard';
import { SubtotalLine } from './components/SubtotalLine';
import { ApprovalCard } from './components/ApprovalCard';
import { ClarificationForm } from './components/ClarificationForm';
import { ConfirmationCard } from './components/ConfirmationCard';

export interface ComponentProps {
  id: string;
  props: Record<string, unknown>;
  children?: React.ReactNode;
}

export const componentRegistry: Record<A2UIComponentType, ComponentType<ComponentProps>> = {
  Column,
  List,
  Text,
  TextInput,
  ProductCard,
  SubtotalLine,
  ApprovalCard,
  ClarificationForm,
  ConfirmationCard,
};

export function renderComponent(
  type: A2UIComponentType,
  id: string,
  props: Record<string, unknown>,
  children?: React.ReactNode,
): React.ReactNode {
  const C = componentRegistry[type];
  if (!C) {
    throw new Error(`A2UI 组件 type=${type} 未注册`);
  }
  return <C id={id} props={props}>{children}</C>;
}
