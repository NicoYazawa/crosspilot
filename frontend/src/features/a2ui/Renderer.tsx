// A2UI 顶层渲染器。
//
// 接收一组 A2UI 报文（按 SSE 事件顺序），构建 surface + components + dataModel，
// 然后渲染根组件。
//
// 状态：
//   - componentsById: id → 组件定义（latest wins；updateComponents 按 id 覆盖）
//   - dataModel:      path → 数据条目列表
//
// 报文顺序契约：
//   createSurface → updateComponents → updateDataModel（三者顺序可错开但 update
//   必须在 create 之后；updateDataModel 之前 createSurface 必须成功）

import React, { useMemo } from 'react';
import { A2UIRenderError, validateMessage } from './schema';
import { renderComponent } from './registry';
import type { ValidatedA2UIMessage, ValidatedComponent } from './schema';
import { SHOPPING_REQUIREMENTS_PATH } from '@/types/a2ui';

export interface RendererProps {
  messages: unknown[];
  onError?: (err: A2UIRenderError) => void;
}

export function A2UIRenderer({ messages, onError }: RendererProps) {
  const state = useA2UIState(messages, onError);
  if (state.kind === 'error') {
    return (
      <div className="a2ui-gray-card" role="alert">
        <strong>A2UI 渲染失败</strong>
        <p>{state.message}</p>
      </div>
    );
  }
  if (state.kind === 'empty') {
    return <p className="muted">等待 Agent 输出…</p>;
  }
  return <>{state.rootElement}</>;
}

type A2UIState =
  | { kind: 'empty' }
  | { kind: 'error'; message: string }
  | { kind: 'ready'; rootElement: React.ReactNode };

interface InternalState {
  components: Map<string, ValidatedComponent>;
  dataModel: Map<string, unknown[]>;
  rootId: string | null;
}

function useA2UIState(messages: unknown[], onError?: (err: A2UIRenderError) => void): A2UIState {
  return useMemo(() => {
    const state: InternalState = {
      components: new Map(),
      dataModel: new Map(),
      rootId: null,
    };

    for (const raw of messages) {
      let msg: ValidatedA2UIMessage;
      try {
        msg = validateMessage(raw);
      } catch (e) {
        if (e instanceof A2UIRenderError) {
          onError?.(e);
          return { kind: 'error', message: e.message };
        }
        throw e;
      }
      applyMessage(state, msg);
    }

    if (!state.rootId) {
      return { kind: 'empty' };
    }
    const rootComponent = state.components.get(state.rootId);
    if (!rootComponent) {
      return { kind: 'error', message: `根组件 ${state.rootId} 不在 components 中` };
    }
    const dataModel = mapToObj(state.dataModel);
    return {
      kind: 'ready',
      rootElement: renderTree(rootComponent, state.components, dataModel),
    };
    // onError 在 hooks 调用中不需要作为依赖；渲染结果只随 messages 变化。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [messages]);
}

function applyMessage(state: InternalState, msg: ValidatedA2UIMessage): void {
  switch (msg.action) {
    case 'createSurface': {
      state.components.clear();
      for (const c of msg.components) {
        state.components.set(c.id, c);
        if (c.path === SHOPPING_REQUIREMENTS_PATH) {
          state.rootId = c.id;
        }
      }
      return;
    }
    case 'updateComponents': {
      for (const c of msg.components) {
        state.components.set(c.id, c);
      }
      return;
    }
    case 'updateDataModel': {
      state.dataModel.set(msg.value.path, msg.value.data);
      return;
    }
  }
}

function mapToObj(m: Map<string, unknown[]>): Record<string, unknown[]> {
  const out: Record<string, unknown[]> = {};
  for (const [k, v] of m) out[k] = v;
  return out;
}

function renderTree(
  component: ValidatedComponent,
  components: Map<string, ValidatedComponent>,
  dataModel: Record<string, unknown[]>,
): React.ReactNode {
  const enrichedProps =
    component.path !== undefined && component.path in dataModel
      ? { ...(component.props ?? {}), data: dataModel[component.path] }
      : component.props ?? {};

  if (component.type === 'Column') {
    const childIds = (component.props?.['children'] as unknown) ?? [];
    const childNodes: React.ReactNode[] = [];
    if (Array.isArray(childIds)) {
      for (const cid of childIds) {
        if (typeof cid !== 'string') continue;
        const child = components.get(cid);
        if (!child) continue;
        childNodes.push(<React.Fragment key={cid}>{renderTree(child, components, dataModel)}</React.Fragment>);
      }
    }
    return renderComponent(component.type, component.id, enrichedProps, childNodes);
  }
  return renderComponent(component.type, component.id, enrichedProps);
}
