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

  // 这条是本文件里唯一能抓住「乐观态提前撤销」的用例，其余都抓不住。
  //
  // 上面那两条都在 `await api.send(...)` **之后**才看 DOM——那时 POST 早已返回、
  // 消息也已由 committed 渲染出来，乐观更新有没有生效根本分辨不出（把
  // startTransition 整段删掉它们照样绿）。而「16ms」那条量的是 send 内部两次
  // performance.now() 的差值，量的是「排队有多快」，不是「什么时候上屏」，
  // 自己给自己打分，同样恒绿。
  //
  // 真实的症状只有把 POST 卡住才看得见：真链路后端是「一次 POST 跑完整轮
  // ReAct」，实测 6–15s。所以这里用一个不自动 resolve 的 fetch 把请求窗口
  // 撑开，在**请求还没回来**的时候断言消息已经在屏幕上。
  it('POST 未返回期间，用户消息就已在屏上（乐观态要覆盖整个请求窗口）', async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const ref: ApiRef = { current: null };
    globalThis.fetch = vi.fn(async () => {
      await gate;
      return new Response(JSON.stringify({ run_id: 'r-slow' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }) as unknown as typeof fetch;

    function W() {
      const a = useChat({ baseUrl: '' });
      ref.current = a;
      return <p data-testid="msgs">{a.messages.map((m) => `${m.role}:${m.content}`).join('|')}</p>;
    }
    flushSync(() => root.render(<W />));
    let pending!: Promise<void>;

    await act(async () => {
      pending = ref.current!.send('找一款防水登山包');
    });

    // 此刻 fetch 仍卡在 gate 上（绝不 resolve），POST 没有返回。
    expect(container.textContent ?? '').toContain('user:找一款防水登山包');
    // isPending 同时决定输入框/发送按钮的禁用与「停止」按钮的可用性，
    // 它提前归 false 会让整个「提交中」的界面状态在真链路上从未出现过。
    expect(ref.current?.isPending).toBe(true);

    await act(async () => {
      release();
      await pending;
    });
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

describe('useChat — 停止', () => {
  // 「停止」原本只调 client.close()，而 client 在 POST 未返回时还是 null——
  // 也就是说它唯一被点得到的时刻（真链路 POST 要跑 6–15s），它作用在一个
  // 空值上，服务端的推理继续跑到结束、token 继续烧。
  it('POST 未返回时点停止，会指名取消这一轮 run', async () => {
    const calls: { url: string; method: string }[] = [];
    let submittedRunId: string | null = null;
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const ref: ApiRef = { current: null };
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push({ url, method: init?.method ?? 'GET' });
      if (url.endsWith('/commerce/ag-ui/run')) {
        submittedRunId = (JSON.parse(String(init?.body)) as { run_id: string }).run_id;
        await gate;
        // 被取消的 run 在真实后端收场为 run_error，POST 随之回 500
        // submit_failed（实测：取消后 3.1s 返回，见改动手记）。
        if (calls.some((c) => c.url.includes('/cancel'))) {
          return new Response(JSON.stringify({ error: 'submit_failed' }), {
            status: 500,
            headers: { 'Content-Type': 'application/json' },
          });
        }
        return new Response(JSON.stringify({ run_id: 'r-server-generated' }), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response('', { status: 200 });
    }) as unknown as typeof fetch;

    function W() {
      const a = useChat({ baseUrl: '' });
      ref.current = a;
      return (
        <p>
          {a.messages.map((m) => `${m.role}:${m.content}`).join('|')}|{a.error}
        </p>
      );
    }
    flushSync(() => root.render(<W />));

    let pending!: Promise<void>;
    await act(async () => {
      pending = ref.current!.send('登山包');
    });

    // 请求体里必须已经带着 run_id：后端声明它是 omitempty，客户端不给就自己生成，
    // 那样「停止」就没有可指的 id——它要到 POST 返回才拿得到，而那时 run 已经跑完。
    expect(submittedRunId, '请求体里没有 run_id').not.toBeNull();

    await act(async () => {
      ref.current!.stop();
    });

    const cancel = calls.find((c) => c.url.includes('/cancel'));
    expect(cancel, '点停止后没有发出取消请求').toBeDefined();
    expect(cancel!.method).toBe('POST');
    // 取消的必须是**这一轮自己给的** id，而不是后端另外生成的那个（r-server-generated）。
    expect(cancel!.url).toContain(encodeURIComponent(submittedRunId!));

    await act(async () => {
      release();
      await pending;
    });
    flushSync(() => root.render(<W />));

    // 主动停止不该看起来像一次崩溃：消息留在屏幕上，提示说的是「已停止」，
    // 而不是一个用户自己按出来的 submit_failed。
    const text = container.textContent ?? '';
    expect(text, '停止后用户自己的消息不该消失').toContain('user:登山包');
    expect(text).toContain('已停止');
    expect(text).not.toContain('submit_failed');
  });
});

describe('useChat — 提交失败时的提示', () => {
  // 之前失败只显示 `submit 503`：状态码既不说原因也不说下一步，而原因
  // （没配模型 key）只躺在服务端日志里。
  it('model_unavailable 给出可操作提示，而不是裸状态码', async () => {
    const ref: ApiRef = { current: null };
    globalThis.fetch = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: 'model_unavailable' }), {
          status: 503,
          headers: { 'Content-Type': 'application/json' },
        }),
    ) as unknown as typeof fetch;

    function W() {
      const a = useChat({ baseUrl: '' });
      ref.current = a;
      return <p data-testid="err">{a.error}</p>;
    }
    flushSync(() => root.render(<W />));
    const api = ref.current;
    if (!api) throw new Error('hook not ready');

    await act(async () => {
      await api.send('找一款防水登山包');
    });
    flushSync(() => root.render(<W />));

    const err = container.textContent ?? '';
    // 断言「指向了要改哪个变量」，而不是点名某一家供应商：写死 QWEN_API_KEY
    // 时，改用 DeepSeek 的人会照着提示去配一个没人读的变量。
    expect(err).toContain('LLM_DEFAULT_PROVIDER');
    expect(err).toContain('API key');
    expect(err).toContain('model_unavailable');
  });

  it('未知错误码回显状态码与错误码', async () => {
    const ref: ApiRef = { current: null };
    globalThis.fetch = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: 'submit_failed' }), {
          status: 500,
          headers: { 'Content-Type': 'application/json' },
        }),
    ) as unknown as typeof fetch;

    function W() {
      const a = useChat({ baseUrl: '' });
      ref.current = a;
      return <p data-testid="err">{a.error}</p>;
    }
    flushSync(() => root.render(<W />));
    const api = ref.current;
    if (!api) throw new Error('hook not ready');

    await act(async () => {
      await api.send('找一款防水登山包');
    });
    flushSync(() => root.render(<W />));

    expect(container.textContent ?? '').toContain('500');
    expect(container.textContent ?? '').toContain('submit_failed');
  });

  // 网关的 HTML 错误页不能让「显示提示」本身崩掉。
  it('响应体不是 JSON 时退回裸状态码', async () => {
    const ref: ApiRef = { current: null };
    globalThis.fetch = vi.fn(
      async () =>
        new Response('<html>502 Bad Gateway</html>', {
          status: 502,
          headers: { 'Content-Type': 'text/html' },
        }),
    ) as unknown as typeof fetch;

    function W() {
      const a = useChat({ baseUrl: '' });
      ref.current = a;
      return <p data-testid="err">{a.error}</p>;
    }
    flushSync(() => root.render(<W />));
    const api = ref.current;
    if (!api) throw new Error('hook not ready');

    await act(async () => {
      await api.send('找一款防水登山包');
    });
    flushSync(() => root.render(<W />));

    expect(container.textContent ?? '').toBe('submit 502');
  });
});
