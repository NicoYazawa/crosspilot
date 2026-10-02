// SSE 客户端集成测试。
//
// 覆盖（G3 验收）：
//   1. seq 缺口：mock fetch 400 + body 'seq_gap' → onError({ kind: 'gap' })
//   2. 跨 run：cursor.runId !== runId → onError({ kind: 'cross-run' })
//   3. 空 token：fetch 调用未带 Authorization 头
//   4. terminal kind：SSE 流以 run_finished 收尾 → polling 不启动
//   5. tail fallback：SSE 流自然关闭 + meta 返回新 last_seq → onTerminal(meta)

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { RecoveringSseClient } from './client';
import type { RunEvent, SseError } from './types';

// 把 SSE 帧字符串装进可读流（手动控制读取时机）。
function sseStream(chunks: string[]): ReadableStream<Uint8Array> {
  const encoder = new TextEncoder();
  let i = 0;
  return new ReadableStream<Uint8Array>({
    pull(controller) {
      if (i >= chunks.length) {
        controller.close();
        return;
      }
      const chunk = chunks[i++];
      if (chunk !== undefined) controller.enqueue(encoder.encode(chunk));
    },
  });
}

// 构造 mock Response：status / headers / body 各自可控。
function makeResponse(
  status: number,
  body: ReadableStream<Uint8Array> | null,
  contentType = 'text/event-stream',
  _url = 'http://localhost/commerce/ag-ui/runs/run-A/events',
): Response {
  const headers = new Headers();
  headers.set('Content-Type', contentType);
  return new Response(body, { status, headers, statusText: status === 200 ? 'OK' : 'Bad Request' });
}

// 把 SSE 帧写入 chunk 数组（每条事件之间用空行分隔）。
function frame(event: string, data: unknown, id?: string): string {
  const lines: string[] = [];
  if (id) lines.push(`id: ${id}`);
  lines.push(`event: ${event}`);
  lines.push(`data: ${JSON.stringify(data)}`);
  return lines.join('\n') + '\n\n';
}

function makeEvent(
  seq: number,
  kind: string,
  extras: Partial<RunEvent> = {},
): RunEvent {
  return {
    event_id: `run-A:${seq}:1`,
    run_id: 'run-A',
    seq,
    kind: kind as RunEvent['kind'],
    created_at: '2026-09-30T00:00:00Z',
    ...extras,
  };
}

let originalFetch: typeof fetch;

beforeEach(() => {
  originalFetch = globalThis.fetch;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

describe('RecoveringSseClient — G3 验收', () => {
  it('seq 缺口：服务端 400 + body seq_gap → onError({ kind: "gap" })', async () => {
    const errors: SseError[] = [];
    globalThis.fetch = vi.fn(async () =>
      new Response('{"error":"seq_gap"}', {
        status: 400,
        headers: { 'Content-Type': 'application/json' },
      }),
    ) as unknown as typeof fetch;

    const client = new RecoveringSseClient({
      baseUrl: 'http://x',
      runId: 'run-A',
      onEvent: () => {},
      onError: (e) => errors.push(e),
    });
    await client.start();
    expect(errors.length).toBe(1);
    const err = errors[0];
    expect(err?.kind).toBe('gap');
  });

  it('跨 run：cursor.runId !== runId → onError({ kind: "cross-run" })', async () => {
    const errors: SseError[] = [];
    const fetchSpy = vi.fn(async () => makeResponse(200, sseStream([]))) as unknown as typeof fetch;
    globalThis.fetch = fetchSpy;

    const client = new RecoveringSseClient({
      baseUrl: 'http://x',
      runId: 'run-B',
      initialCursor: { runId: 'run-A', seq: 3 },
      onEvent: () => {},
      onError: (e) => errors.push(e),
    });
    await client.start();
    expect(errors.length).toBe(1);
    const err = errors[0];
    expect(err?.kind).toBe('cross-run');
    if (err?.kind === 'cross-run') {
      expect(err.cursorRunId).toBe('run-A');
      expect(err.targetRunId).toBe('run-B');
    }
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('空 token：fetch 调用未带 Authorization 头', async () => {
    const seenHeaders: Headers[] = [];
    globalThis.fetch = vi.fn(async (_url, init) => {
      const h = new Headers(init?.headers);
      seenHeaders.push(h);
      return makeResponse(200, sseStream([]));
    }) as unknown as typeof fetch;

    const client = new RecoveringSseClient({
      baseUrl: 'http://x',
      runId: 'run-A',
      authToken: '',
      onEvent: () => {},
      onError: () => {},
    });
    await client.start();
    const h = seenHeaders[0];
    expect(h).toBeDefined();
    if (h) {
      expect(h.get('Authorization')).toBeNull();
      expect(h.get('X-Session-ID')).toBeNull();
    }
  });

  it('带 token：fetch 调用注入 Authorization: Bearer', async () => {
    const seenHeaders: Headers[] = [];
    globalThis.fetch = vi.fn(async (_url, init) => {
      seenHeaders.push(new Headers(init?.headers));
      return makeResponse(200, sseStream([]));
    }) as unknown as typeof fetch;

    const client = new RecoveringSseClient({
      baseUrl: 'http://x',
      runId: 'run-A',
      authToken: 'jwt-abc',
      sessionId: 'sess-1',
      onEvent: () => {},
      onError: () => {},
    });
    await client.start();
    const h = seenHeaders[0];
    expect(h?.get('Authorization')).toBe('Bearer jwt-abc');
    expect(h?.get('X-Session-ID')).toBe('sess-1');
  });

  it('SSE 与 meta 请求都走 /commerce/ag-ui 前缀', async () => {
    const seenUrls: string[] = [];
    globalThis.fetch = vi.fn(async (url) => {
      seenUrls.push(String(url));
      return makeResponse(200, sseStream([]));
    }) as unknown as typeof fetch;

    const client = new RecoveringSseClient({
      baseUrl: 'http://x',
      runId: 'run-A',
      onEvent: () => {},
      onError: () => {},
    });
    await client.start();
    expect(seenUrls[0]).toBe('http://x/commerce/ag-ui/runs/run-A/events');
  });

  it('terminal kind：SSE 以 run_finished 收尾 → polling 不启动', async () => {
    vi.useFakeTimers();
    const events: RunEvent[] = [];
    const fetchSpy = vi.fn(async () =>
      makeResponse(200, sseStream([
        frame('run_start', makeEvent(0, 'run_start')),
        frame('model_turn', makeEvent(1, 'model_turn')),
        frame('run_finished', makeEvent(2, 'run_finished')),
      ])),
    ) as unknown as typeof fetch;
    globalThis.fetch = fetchSpy;

    const client = new RecoveringSseClient({
      baseUrl: 'http://x',
      runId: 'run-A',
      onEvent: (e) => events.push(e),
      onError: () => {},
      pollMeta: { intervalMs: 1000, onTerminal: () => {} },
    });
    await client.start();
    await vi.advanceTimersByTimeAsync(3000);
    expect(events.map((e) => e.kind)).toEqual(['run_start', 'model_turn', 'run_finished']);
    // SSE 一次性推完 + 终端 kind → 不应再调 fetch（即 polling 没启动）
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    vi.useRealTimers();
  });

  it('tail fallback：SSE 自然关闭 + meta 返回新 last_seq → onTerminal(meta)', async () => {
    vi.useFakeTimers();
    const events: RunEvent[] = [];
    const terminalMetas: { run_id: string; last_seq: number }[] = [];
    let callIndex = 0;
    globalThis.fetch = vi.fn(async () => {
      callIndex++;
      if (callIndex === 1) {
        // SSE 流：一条 run_start（未到终态），流自然关闭
        return makeResponse(200, sseStream([
          frame('run_start', makeEvent(0, 'run_start')),
        ]));
      }
      // meta 轮询：返回不同 last_seq
      return new Response(
        JSON.stringify({ run_id: 'run-A', last_seq: 5 }),
        { status: 200, headers: { 'Content-Type': 'application/json' } },
      );
    }) as unknown as typeof fetch;

    const client = new RecoveringSseClient({
      baseUrl: 'http://x',
      runId: 'run-A',
      onEvent: (e) => events.push(e),
      onError: () => {},
      pollMeta: {
        intervalMs: 1000,
        onTerminal: (m) => terminalMetas.push(m),
      },
    });
    await client.start();
    await vi.advanceTimersByTimeAsync(1500);
    expect(events.map((e) => e.seq)).toEqual([0]);
    expect(terminalMetas.length).toBeGreaterThanOrEqual(1);
    expect(terminalMetas[0]?.last_seq).toBe(5);
    vi.useRealTimers();
  });
});
