// useChat 测试 — G1 验收：useOptimistic 上屏延迟 < 16ms。
//
// 策略：在 jsdom + flushSync 下，调用 send() 触发的 addOptimistic 应该在
// 同一个 microtask 内反映到 messages。onMetric 回调拿到 (t1-t0) 的差值。
//
// 实现注：useChat 返回的 api 对象每次 render 都换，因此测试不能持有旧引用。
// 改用「组件渲染 + DOM 断言」+ 一个 ref-style 容器始终指向最新 api。

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createRoot, type Root } from 'react-dom/client';
import { flushSync } from 'react-dom';
import { act } from 'react';
import { useChat } from './useChat';

interface ApiRef {
  current: ReturnType<typeof useChat> | null;
}

let container: HTMLDivElement;
let root: Root;
let originalFetch: typeof fetch;

beforeEach(() => {
  container = document.createElement('div');
  document.body.appendChild(container);
  root = createRoot(container);
  originalFetch = globalThis.fetch;
});

afterEach(() => {
  root.unmount();
  container.remove();
  globalThis.fetch = originalFetch;
});

describe('useChat — G1 验收', () => {
  it('send() 后乐观消息立即上屏', async () => {
    const ref: ApiRef = { current: null };
    globalThis.fetch = vi.fn(async () =>
      new Response(JSON.stringify({ run_id: 'r-test' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ) as unknown as typeof fetch;

    function W() {
      const a = useChat({ baseUrl: '' });
      ref.current = a;
      return (
        <div>
          <p data-testid="msgs">{a.messages.map((m) => `${m.role}:${m.content}`).join('|')}</p>
        </div>
      );
    }
    flushSync(() => root.render(<W />));
    const api = ref.current;
    if (!api) throw new Error('hook not ready');

    await act(async () => {
      await api.send('找一款防水登山包');
    });

    // re-render after state change
    flushSync(() => root.render(<W />));
    const dom = container.textContent ?? '';
    expect(dom).toContain('user:找一款防水登山包');
  });

  it('addOptimistic 延迟 < 16ms（onMetric 收到的差值）', async () => {
    const latencies: number[] = [];
    const ref: ApiRef = { current: null };
    globalThis.fetch = vi.fn(async () =>
      new Response(JSON.stringify({ run_id: 'r1' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    ) as unknown as typeof fetch;

    function W() {
      const a = useChat({
        baseUrl: '',
        onMetric: (name, value) => {
          if (name === 'crosspilot.optimistic.add_latency_ms') latencies.push(value);
        },
      });
      ref.current = a;
      return <div>{a.messages.length}</div>;
    }
    flushSync(() => root.render(<W />));
    const api = ref.current;
    if (!api) throw new Error('hook not ready');

    await act(async () => {
      await api.send('hello');
    });

    expect(latencies.length).toBeGreaterThan(0);
    const latency = latencies[0];
    if (latency === undefined) throw new Error('no latency recorded');
    // G1：< 16ms（单帧预算）
    expect(latency).toBeLessThan(16);
  });
});
