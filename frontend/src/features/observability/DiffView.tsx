// DiffView：两条 run 的事件序列对比（F2）。
//
// 数据：GET /observability/runs/{baseline}/diff?against={against}
// 颜色编码：added=绿 / removed=红 / changed=黄 / unchanged=灰

import { useEffect, useState } from 'react';
import { ApiError, defaultApi } from '@/lib/api/client';
import type { DiffResult } from '@/types/api';

export interface DiffViewProps {
  baselineRunId: string;
  againstRunId: string;
}

export function DiffView({ baselineRunId, againstRunId }: DiffViewProps) {
  const [data, setData] = useState<DiffResult | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    defaultApi()
      .get<DiffResult>(`/observability/runs/${encodeURIComponent(baselineRunId)}/diff`, {
        against: againstRunId,
      })
      .then((r) => {
        if (alive) setData(r);
      })
      .catch((e: unknown) => {
        if (!alive) return;
        if (e instanceof ApiError) setErr(`${e.status}: ${e.body}`);
        else setErr(String(e));
      });
    return () => {
      alive = false;
    };
  }, [baselineRunId, againstRunId]);

  if (baselineRunId === againstRunId) {
    return <p className="error">baseline 与 against 不能相同</p>;
  }
  if (err) {
    return <p className="error">diff 失败：{err}</p>;
  }
  if (data === null) {
    return <p className="muted">加载对比…</p>;
  }
  return (
    <div className="diff-view">
      <div className="diff-badges">
        <span className="badge badge-added">added: {data.added}</span>
        <span className="badge badge-removed">removed: {data.removed}</span>
        <span className="badge badge-changed">changed: {data.changed}</span>
        <span className="badge badge-unchanged">unchanged: {data.unchanged}</span>
      </div>
      <table className="diff-table">
        <thead>
          <tr>
            <th>seq</th>
            <th>kind</th>
            <th>reason</th>
          </tr>
        </thead>
        <tbody>
          {data.items.length === 0 ? (
            <tr>
              <td colSpan={3} className="muted">无差异项</td>
            </tr>
          ) : null}
          {data.items.map((it) => (
            <tr key={`${it.seq}:${it.kind}`} className={`diff-row diff-${it.kind}`}>
              <td>[{it.seq}]</td>
              <td>{it.kind}</td>
              <td>{it.reason}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
