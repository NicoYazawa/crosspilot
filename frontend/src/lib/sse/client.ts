// 可恢复 SSE 客户端。
//
// 设计要点：
//   1. 协议：EventSource 原生 API 不支持自定义请求头 + body，所以用 fetch +
//      ReadableStream 自行解析 SSE 帧。HMAC 头由 buildHmacHeaders 注入。
//   2. 跨 run 拒绝：进入 connect 前用 bindToRun 校验 cursor.runId === runId。
//   3. seq 缺口：每条事件 seq 必须 == lastSeq+1，否则调用 onError(gap) 并停。
//   4. live tail fallback：SSE 解析到流尾（服务端关闭连接）时启动 polling，
//      见到 TERMINAL_KINDS 调 onTerminal(meta) 由调用方清理资源。
//   5. 终止语义：见到 TERMINAL_KINDS 立即关流，不再触发 fallback。
//
// 不在本文件范围：
//   - 重连退避策略（P7 横切测试加）
//   - 事件持久化（IndexedDB 缓存 P7 加）

import type {
  Cursor,
  ParsedSseFrame,
  RunEvent,
  RunEventKind,
  SseClientOptions,
  SseError,
} from './types';
import { TERMINAL_KINDS } from './types';
import { bindToRun, parseCursor } from './cursor';
import { buildHmacHeaders } from './hmac';

interface ActiveState {
  abortController: AbortController;
  lastSeq: number;
  pollTimer: ReturnType<typeof setInterval> | null;
  closed: boolean;
}

export class RecoveringSseClient {
  private readonly opts: SseClientOptions;
  private state: ActiveState | null = null;

  constructor(opts: SseClientOptions) {
    this.opts = opts;
  }

  // 启动一次订阅。
  //
  // 同实例多次调用 start() 会先关旧的（重置 cursor 与 lastSeq 仍以构造时为准）。
  // 调用方应保证：start() 与 close() 成对；多次 start 用于「同 run 换 cursor 重连」。
  async start(initialCursor?: Cursor | null): Promise<void> {
    this.close();
    const cursor = initialCursor !== undefined ? initialCursor : this.opts.initialCursor ?? null;

    const ac = new AbortController();
    if (this.opts.signal) {
      if (this.opts.signal.aborted) {
        return;
      }
      this.opts.signal.addEventListener('abort', () => ac.abort(), { once: true });
    }

    // state 必须先建好，否则 bindToRun 抛错时 reportError 会被 state=null 守卫吞掉。
    this.state = {
      abortController: ac,
      lastSeq: -1,
      pollTimer: null,
      closed: false,
    };

    // 跨 run 拒绝：cursor.runId 必须与目标 runId 一致。
    let since: number;
    try {
      since = bindToRun(cursor, this.opts.runId);
    } catch (err) {
      const e = err as { name?: string; cursorRunId?: string; targetRunId?: string };
      if (e.name === 'CrossRunError') {
        this.reportError({
          kind: 'cross-run',
          cursorRunId: e.cursorRunId ?? '',
          targetRunId: e.targetRunId ?? '',
        });
      }
      this.close();
      return;
    }
    this.state.lastSeq = since === 0 ? -1 : since - 1;

    try {
      await this.connect(since);
    } catch (err) {
      if (this.state && !this.state.closed) {
        this.reportError({ kind: 'network', cause: err });
      }
    }
  }

  // 主动关闭：abort fetch + 停 polling + 标记 closed 防 onError 二次触发。
  close(): void {
    if (!this.state) return;
    this.state.closed = true;
    if (this.state.pollTimer !== null) {
      clearInterval(this.state.pollTimer);
      this.state.pollTimer = null;
    }
    this.state.abortController.abort();
    this.state = null;
  }

  private async connect(since: number): Promise<void> {
    const state = this.state;
    if (!state) return;

    const path = `/agui/runs/${encodeURIComponent(this.opts.runId)}/events`;
    const url = new URL(path, this.opts.baseUrl);
    if (since > 0) {
      url.searchParams.set('cursor', `${this.opts.runId}:${since - 1}`);
    }

    const headers: Record<string, string> = {
      Accept: 'text/event-stream',
    };
    if (this.opts.hmacSecret) {
      const hmacHeaders = await buildHmacHeaders({
        method: 'GET',
        path: url.pathname,
        secret: this.opts.hmacSecret,
      });
      Object.assign(headers, hmacHeaders);
    }

    const response = await fetch(url.toString(), {
      method: 'GET',
      headers,
      signal: state.abortController.signal,
    });

    if (!response.ok) {
      const body = await safeReadText(response);
      // 服务端用 {"error":"seq_gap"} 标识 gap 拒绝。
      if (response.status === 400 && body.includes('seq_gap')) {
        const last = state.lastSeq;
        this.reportError({ kind: 'gap', expected: last + 1, actual: last });
      } else {
        this.reportError({ kind: 'http', status: response.status, body });
      }
      return;
    }
    if (!response.body) {
      this.reportError({ kind: 'network', cause: new Error('response body is null') });
      return;
    }

    await this.parseStream(response.body);
    if (this.state && !this.state.closed) {
      this.startPolling();
    }
  }

  private async parseStream(body: ReadableStream<Uint8Array>): Promise<void> {
    const reader = body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    let pendingId = '';
    let pendingEvent = '';
    let pendingData: string[] = [];

    const flushFrame = (): void => {
      if (pendingEvent === '' && pendingData.length === 0) return;
      const frame: ParsedSseFrame = {
        id: pendingId,
        event: pendingEvent,
        data: pendingData.join('\n'),
      };
      pendingId = '';
      pendingEvent = '';
      pendingData = [];
      this.handleFrame(frame);
    };

    try {
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let sep: number;
        while ((sep = buffer.indexOf('\n\n')) !== -1) {
          const raw = buffer.slice(0, sep);
          buffer = buffer.slice(sep + 2);
          this.parseFrameRaw(raw, (line) => {
            if (line.startsWith('id:')) pendingId = line.slice(3).trim();
            else if (line.startsWith('event:')) pendingEvent = line.slice(6).trim();
            else if (line.startsWith('data:')) pendingData.push(line.slice(5).trim());
          });
          flushFrame();
          if (this.state?.closed) return;
        }
      }
    } finally {
      reader.releaseLock();
    }
  }

  private parseFrameRaw(raw: string, onLine: (line: string) => void): void {
    for (const line of raw.split('\n')) {
      onLine(line);
    }
  }

  private handleFrame(frame: ParsedSseFrame): void {
    if (!frame.event) return;
    let parsed: unknown;
    try {
      parsed = JSON.parse(frame.data as string);
    } catch {
      parsed = {};
    }
    const ev = parsed as Partial<RunEvent>;
    if (typeof ev.seq !== 'number' || typeof ev.kind !== 'string' || typeof ev.run_id !== 'string') {
      return;
    }
    const kind = ev.kind as RunEventKind;
    const state = this.state;
    if (!state) return;

    // seq 缺口拒绝：必须严格 +1。
    if (ev.seq !== state.lastSeq + 1 && !(state.lastSeq === -1 && ev.seq === 0)) {
      this.reportError({ kind: 'gap', expected: state.lastSeq + 1, actual: ev.seq });
      this.close();
      return;
    }
    state.lastSeq = ev.seq;

    const runEvent: RunEvent = {
      event_id: ev.event_id ?? frame.id,
      run_id: ev.run_id,
      seq: ev.seq,
      kind,
      payload: ev.payload,
      created_at: ev.created_at ?? new Date().toISOString(),
      ...(ev.agent !== undefined ? { agent: ev.agent } : {}),
    };
    this.opts.onEvent(runEvent);

    if (TERMINAL_KINDS.has(kind)) {
      // 终端 kind：直接关闭，不再触发 polling fallback。
      this.close();
    }
  }

  private startPolling(): void {
    const state = this.state;
    const pollMeta = this.opts.pollMeta;
    if (!state || !pollMeta) return;

    const tick = async (): Promise<void> => {
      if (state.closed) return;
      try {
        const metaUrl = new URL(`/agui/runs/${encodeURIComponent(this.opts.runId)}`, this.opts.baseUrl);
        const headers: Record<string, string> = { Accept: 'application/json' };
        if (this.opts.hmacSecret) {
          const hmacHeaders = await buildHmacHeaders({
            method: 'GET',
            path: metaUrl.pathname,
            secret: this.opts.hmacSecret,
          });
          Object.assign(headers, hmacHeaders);
        }
        const res = await fetch(metaUrl.toString(), {
          method: 'GET',
          headers,
          signal: state.abortController.signal,
        });
        if (!res.ok) return;
        const meta = (await res.json()) as { run_id: string; last_seq: number };
        if (meta.last_seq !== state.lastSeq) {
          // 有新事件没拉全：触发重连流程。
          // 简化策略：通知调用方「应从头订阅」并停止 polling。
          if (state.pollTimer !== null) {
            clearInterval(state.pollTimer);
            state.pollTimer = null;
          }
          pollMeta.onTerminal({ run_id: meta.run_id, last_seq: meta.last_seq });
        }
      } catch {
        // 网络抖动：下个 tick 继续
      }
    };

    state.pollTimer = setInterval(() => {
      void tick();
    }, pollMeta.intervalMs);
  }

  private reportError(err: SseError): void {
    if (!this.state || this.state.closed) return;
    this.opts.onError(err);
  }
}

async function safeReadText(res: Response): Promise<string> {
  try {
    return await res.text();
  } catch {
    return '';
  }
}

export { parseCursor };
