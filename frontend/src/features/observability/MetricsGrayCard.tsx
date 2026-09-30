// MetricsGrayCard：观测通道计数（F10）。
//
// 数据：GET /observability/metrics
// 容器未装配 → 503 → 显示「未启用」灰卡
// 已装配 → 显示快照数据

import { useEffect, useState } from 'react';
import { ApiError, defaultApi } from '@/lib/api/client';
import type { MetricsSnapshot } from '@/types/api';

export function MetricsGrayCard() {
  const [snapshot, setSnapshot] = useState<MetricsSnapshot | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    defaultApi()
      .get<MetricsSnapshot>(`/observability/metrics`)
      .then((s) => {
        if (alive) setSnapshot(s);
      })
      .catch((e: unknown) => {
        if (!alive) return;
        if (e instanceof ApiError && e.status === 503) {
          setUnavailable(true);
          return;
        }
        setErr(e instanceof Error ? e.message : String(e));
      });
    return () => {
      alive = false;
    };
  }, []);

  if (err) return <p className="error">metrics 加载失败：{err}</p>;
  if (unavailable) {
    return (
      <div className="metrics-gray-card">
        <strong>metrics 未启用</strong>
        <p className="muted">后端容器未装配 metrics 通道；P9 接入时再启用</p>
      </div>
    );
  }
  if (snapshot === null) {
    return <p className="muted">加载 metrics…</p>;
  }
  return (
    <div className="metrics-snapshot">
      <h3>观测通道计数</h3>
      <ul>
        <li>queue_depth: {snapshot.queue_depth}</li>
        <li>dropped_total: {snapshot.dropped_total}</li>
        <li>emit_total: {snapshot.emit_total}</li>
        <li>emit_errors_total: {snapshot.emit_errors_total}</li>
        <li>redact_errors: {snapshot.redact_errors}</li>
      </ul>
    </div>
  );
}
