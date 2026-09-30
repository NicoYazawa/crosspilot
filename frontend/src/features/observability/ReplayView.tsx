// ReplayView：事件时间线（F1 回放）。
//
// 数据：GET /observability/runs/{runId}/events?from=0&limit=200
//
// 渲染策略：
//   - 每行 [seq] [kind] [agent] [payload 摘要]
//   - 含 <REDACTED> 字样的字段旁显 ⛔ 徽标（P5 redact 产物）

import { useEffect, useState } from 'react';
import { ApiError, defaultApi } from '@/lib/api/client';
import type { RunEvent } from '@/types/agui';

interface ReplayResponse {
  run_id: string;
  events: RunEvent[];
  total_seq: number;
  has_more: boolean;
}

export interface ReplayViewProps {
  runId: string;
  limit?: number;
}

export function ReplayView({ runId, limit = 200 }: ReplayViewProps) {
  const [events, setEvents] = useState<RunEvent[] | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    defaultApi()
      .get<ReplayResponse>(`/observability/runs/${encodeURIComponent(runId)}/events`, {
        from: 0,
        limit,
      })
      .then((r) => {
        if (alive) setEvents(r.events);
      })
      .catch((e: unknown) => {
        if (!alive) return;
        if (e instanceof ApiError) setErr(`${e.status}: ${e.body}`);
        else setErr(String(e));
      });
    return () => {
      alive = false;
    };
  }, [runId, limit]);

  if (err) {
    return <p className="error">回放失败：{err}</p>;
  }
  if (events === null) {
    return <p className="muted">加载事件…</p>;
  }
  if (events.length === 0) {
    return <p className="muted">该 run 无事件记录</p>;
  }
  return (
    <div className="replay-view">
      <p className="muted">共 {events.length} 条</p>
      <ol className="timeline">
        {events.map((ev) => (
          <li key={`${ev.run_id}:${ev.seq}`}>
            <span className="seq">[{ev.seq}]</span>{' '}
            <span className={`kind kind-${ev.kind}`}>{ev.kind}</span>{' '}
            {ev.agent ? <span className="agent">@ {ev.agent}</span> : null}{' '}
            <span className="payload">{summarizePayload(ev.payload)}</span>
            {containsRedacted(ev.payload) ? <span className="redact-badge">⛔ REDACTED</span> : null}
          </li>
        ))}
      </ol>
    </div>
  );
}

function summarizePayload(p: unknown): string {
  if (p === null || p === undefined) return '';
  if (typeof p === 'string') return p;
  try {
    const s = JSON.stringify(p);
    return s.length > 120 ? s.slice(0, 117) + '...' : s;
  } catch {
    return '[unserializable]';
  }
}

function containsRedacted(p: unknown): boolean {
  if (typeof p === 'string') return p.includes('<REDACTED>');
  if (p && typeof p === 'object') {
    return JSON.stringify(p).includes('<REDACTED>');
  }
  return false;
}
