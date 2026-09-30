// 选购流程核心 hook：useOptimistic + useTransition + SSE 订阅。
//
// 设计要点：
//   - 用户消息走 useOptimistic：发送时立即上屏，< 16ms 上屏延迟（G1 验收）
//   - 服务端消息（model_turn / a2ui）走 SSE 流，不走乐观（避免与真实事件冲突）
//   - 提交时机：fetch POST /agui/runs 返回 + run_id 拿到后，把 optimistic
//     标记为已提交（仍是 useOptimistic 的特性：transition 结束时回到 committed）

import { useCallback, useOptimistic, useState, useTransition } from 'react';
import type { RunEvent } from '@/types/agui';
import { RecoveringSseClient } from '@/lib/sse/client';
import type { SseError } from '@/lib/sse/types';

export interface ChatMessage {
  id: string;
  role: 'user' | 'agent';
  content: string;
  pending: boolean;
}

export interface UseChatOptions {
  baseUrl: string;
  hmacSecret?: string;
  onMetric?: (name: string, value: number, tags?: Record<string, string>) => void;
}

export interface UseChatResult {
  messages: ChatMessage[];
  isPending: boolean;
  error: string | null;
  send: (text: string) => Promise<void>;
  stop: () => void;
}

interface SubmitRequest {
  buyer_id: string;
  session_id: string;
  query: string;
  agent: string;
}

interface SubmitResponse {
  run_id: string;
}

const LATENCY_METRIC = 'crosspilot.optimistic.add_latency_ms';

export function useChat(opts: UseChatOptions): UseChatResult {
  const [committed, setCommitted] = useState<ChatMessage[]>([]);
  const [optimistic, addOptimistic] = useOptimistic<ChatMessage[], ChatMessage>(
    committed,
    (curr, next) => [...curr, next],
  );
  const [isPending, startTransition] = useTransition();
  const [error, setError] = useState<string | null>(null);
  const [client, setClient] = useState<RecoveringSseClient | null>(null);

  const onEvent = useCallback((ev: RunEvent): void => {
    if (ev.kind === 'model_turn') {
      const payload = (ev.payload ?? {}) as { content?: string };
      const text = typeof payload.content === 'string' ? payload.content : '';
      if (text !== '') {
        setCommitted((prev) => [...prev, { id: ev.event_id, role: 'agent', content: text, pending: false }]);
      }
    }
    if (ev.kind === 'a2ui') {
      // A2UI 渲染由 Renderer 子组件处理；这里只记录一条简略文本
      const payload = (ev.payload ?? {}) as { action?: string };
      setCommitted((prev) => [...prev, {
        id: ev.event_id,
        role: 'agent',
        content: `[A2UI ${payload.action ?? 'message'}]`,
        pending: false,
      }]);
    }
  }, []);

  const onError = useCallback((err: SseError): void => {
    setError(`${err.kind}${err.kind === 'gap' ? ` expected=${err.expected} actual=${err.actual}` : ''}`);
  }, []);

  const send = useCallback(async (text: string): Promise<void> => {
    setError(null);
    const t0 = performance.now();
    const msg: ChatMessage = {
      id: `pending-${Date.now()}`,
      role: 'user',
      content: text,
      pending: true,
    };

    startTransition(() => {
      addOptimistic(msg);
    });
    const t1 = performance.now();
    opts.onMetric?.(LATENCY_METRIC, t1 - t0);

    // 真实提交
    let runId: string;
    try {
      const res = await fetch(`${opts.baseUrl}/agui/runs`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          buyer_id: 'demo-buyer',
          session_id: 'demo-session',
          query: text,
          agent: 'shopping',
        } satisfies SubmitRequest),
      });
      if (!res.ok) {
        setError(`submit ${res.status}`);
        return;
      }
      const data = (await res.json()) as SubmitResponse;
      runId = data.run_id;
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      return;
    }

    // 把乐观消息标记为已提交
    setCommitted((prev) => [...prev, { ...msg, pending: false }]);

    // 启动 SSE 订阅
    const sseOpts: ConstructorParameters<typeof RecoveringSseClient>[0] = {
      baseUrl: opts.baseUrl,
      runId,
      onEvent,
      onError,
    };
    if (opts.hmacSecret !== undefined) sseOpts.hmacSecret = opts.hmacSecret;
    const c = new RecoveringSseClient(sseOpts);
    setClient(c);
    void c.start();
    // addOptimistic / startTransition 来自 useOptimistic / useTransition，
    // 引用稳定；显式省略避免 exhaustive-deps 噪音。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [opts, onEvent, onError]);

  const stop = useCallback((): void => {
    client?.close();
    setClient(null);
  }, [client]);

  return {
    messages: optimistic,
    isPending,
    error,
    send,
    stop,
  };
}
