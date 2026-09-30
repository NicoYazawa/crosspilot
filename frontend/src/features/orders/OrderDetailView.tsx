// 订单详情视图。
//
// 数据源：
//   - GET /observability/runs/{runId}/events?from=0&limit=200 → 时间线
//   - GET /observability/runs/{runId}/cost                    → 成本摘要
//
// Step 3 仅做基础数据接入与时间线渲染；Step 5 把 ReplayView / DiffView /
// CostView / ABView / MetricsGrayCard 拆为独立子组件并补全图表。

import { useEffect, useState } from 'react';
import { useParams } from 'react-router-dom';
import { ApiError, defaultApi } from '@/lib/api/client';
import type { CostSummary } from '@/types/api';
import type { RunEvent } from '@/types/agui';

interface ReplayResponse {
  run_id: string;
  events: RunEvent[];
  total_seq: number;
  has_more: boolean;
}

export function OrderDetailView() {
  const { runId } = useParams<{ runId: string }>();
  const [events, setEvents] = useState<RunEvent[] | null>(null);
  const [cost, setCost] = useState<CostSummary | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    if (!runId) return;
    let alive = true;
    const api = defaultApi();
    Promise.all([
      api
        .get<ReplayResponse>(`/observability/runs/${encodeURIComponent(runId)}/events`, { from: 0, limit: 200 })
        .catch((e: unknown) => {
          if (e instanceof ApiError) throw new Error(`events ${e.status}: ${e.body}`);
          throw e;
        }),
      api.get<CostSummary>(`/observability/runs/${encodeURIComponent(runId)}/cost`).catch((e: unknown) => {
        if (e instanceof ApiError && e.status === 404) return null;
        throw e;
      }),
    ])
      .then(([replay, costData]) => {
        if (!alive) return;
        setEvents(replay.events);
        setCost(costData);
      })
      .catch((e: unknown) => {
        if (!alive) return;
        setErr(e instanceof Error ? e.message : String(e));
      });
    return () => {
      alive = false;
    };
  }, [runId]);

  if (!runId) {
    return (
      <section>
        <h1>订单详情</h1>
        <p>runId 缺失</p>
      </section>
    );
  }
  if (err) {
    return (
      <section>
        <h1>订单详情</h1>
        <p>runId: {runId}</p>
        <p className="error">加载失败：{err}</p>
      </section>
    );
  }
  if (events === null) {
    return (
      <section>
        <h1>订单详情</h1>
        <p>runId: {runId}</p>
        <p>加载中…</p>
      </section>
    );
  }

  return (
    <section>
      <h1>订单详情</h1>
      <p>runId: {runId}</p>

      <section className="cost-summary">
        <h2>成本</h2>
        {cost === null ? (
          <p>成本数据未挂载</p>
        ) : (
          <ul>
            <li>总成本：{cost.total_cost_minor} {cost.currency}</li>
            <li>未定价调用：{cost.unpriced_count}</li>
            <li>总调用数：{cost.total_calls}</li>
            <li>tokens: in={cost.tokens_in} / out={cost.tokens_out} / cached={cost.tokens_cached} / reasoning={cost.tokens_reasoning}</li>
          </ul>
        )}
      </section>

      <section className="timeline">
        <h2>事件时间线（共 {events.length} 条）</h2>
        <ol>
          {events.map((ev) => (
            <li key={`${ev.run_id}:${ev.seq}`}>
              <span className="seq">[{ev.seq}]</span>{' '}
              <span className={`kind kind-${ev.kind}`}>{ev.kind}</span>{' '}
              {ev.agent ? <span className="agent">@ {ev.agent}</span> : null}
            </li>
          ))}
        </ol>
      </section>
    </section>
  );
}
