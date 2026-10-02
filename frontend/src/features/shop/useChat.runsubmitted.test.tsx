// useChat — onRunSubmitted 回调。
//
// 存在的理由：后端没有「列出我的 run」的接口，订单页与历史页只能读前端自己
// 记下的 run_id。这个回调是那条记录的唯一起点——此前 recordRun 没有任何调用方，
// 两页因此永远是空表。

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createRoot, type Root } from 'react-dom/client';
import { flushSync } from 'react-dom';
import { act } from 'react';
import { useChat } from './useChat';

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

// 只回 POST，SSE 的 GET 一律挂起（订阅成功与否不影响本文件断言）。
function stubFetch(runId: string) {
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const url = String(input);
    if (url.includes('/runs/') && url.endsWith('/events')) {
      return new Promise<Response>(() => {}); // 挂起，不结束
    }
    return new Response(JSON.stringify({ run_id: runId }), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  }) as unknown as typeof fetch;
}

describe('useChat — onRunSubmitted', () => {
  it('POST 成功后回调一次，带服务端回显的 run_id 与原始 query', async () => {
    stubFetch('run-server-1');
    const calls: Array<[string, string]> = [];
    const ref: { current: ReturnType<typeof useChat> | null } = { current: null };

    function W() {
      const a = useChat({
        baseUrl: '',
        onRunSubmitted: (runId, query) => calls.push([runId, query]),
      });
      ref.current = a;
      return <p>{a.error ?? ''}</p>;
    }
    flushSync(() => root.render(<W />));
    const api = ref.current;
    if (!api) throw new Error('hook not ready');

    await act(async () => {
      await api.send('找一款防水登山包');
    });

    expect(calls).toHaveLength(1);
    // run_id 以服务端回显为准，而不是客户端自己生成的那个
    expect(calls[0]?.[0]).toBe('run-server-1');
    expect(calls[0]?.[1]).toBe('找一款防水登山包');
  });

  it('POST 失败时不回调——没有 run_id 就没有可回看的记录', async () => {
    globalThis.fetch = vi.fn(async () =>
      new Response(JSON.stringify({ error: 'submit_failed' }), { status: 500 }),
    ) as unknown as typeof fetch;
    const calls: string[] = [];

    const ref: { current: ReturnType<typeof useChat> | null } = { current: null };
    function W() {
      const a = useChat({ baseUrl: '', onRunSubmitted: (runId) => calls.push(runId) });
      ref.current = a;
      return <p>{a.error ?? ''}</p>;
    }
    flushSync(() => root.render(<W />));
    const api = ref.current;
    if (!api) throw new Error('hook not ready');

    await act(async () => {
      await api.send('找一款防水登山包');
    });

    expect(calls).toHaveLength(0);
  });

  it('回调抛错不会让一轮对话失败', async () => {
    stubFetch('run-server-2');
    const errSpy = vi.spyOn(console, 'error').mockImplementation(() => {});

    const ref: { current: ReturnType<typeof useChat> | null } = { current: null };
    function W() {
      const a = useChat({
        baseUrl: '',
        onRunSubmitted: () => {
          throw new Error('IndexedDB 写不进去');
        },
      });
      ref.current = a;
      return <p data-testid="err">{a.error ?? ''}</p>;
    }
    flushSync(() => root.render(<W />));
    const api = ref.current;
    if (!api) throw new Error('hook not ready');

    await act(async () => {
      await api.send('找一款防水登山包');
    });
    flushSync(() => root.render(<W />));

    // 写本地历史失败是辅助功能的问题，不该把主链路也拖红。
    expect(container.textContent ?? '').toBe('');
    expect(errSpy).toHaveBeenCalled();
    errSpy.mockRestore();
  });
});
