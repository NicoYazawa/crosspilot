// ABView：A/B 实验臂面板（F8 不渲染 judge_score 列）。
//
// 数据：GET /observability/experiments/{key}/arms
// 404 → 显示「实验未找到」灰卡（Plan R8 缓解）

import { useEffect, useState } from 'react';
import { ApiError, defaultApi } from '@/lib/api/client';
import type { ArmSummary } from '@/types/api';

interface ExperimentArmsResponse {
  experiment: string;
  arms: ArmSummary[];
  fetched_at: string;
}

export interface ABViewProps {
  experimentKey: string;
}

export function ABView({ experimentKey }: ABViewProps) {
  const [data, setData] = useState<ExperimentArmsResponse | null>(null);
  const [err, setErr] = useState<{ status: number; body: string } | null>(null);

  useEffect(() => {
    let alive = true;
    defaultApi()
      .get<ExperimentArmsResponse>(`/observability/experiments/${encodeURIComponent(experimentKey)}/arms`)
      .then((r) => {
        if (alive) setData(r);
      })
      .catch((e: unknown) => {
        if (!alive) return;
        if (e instanceof ApiError) setErr({ status: e.status, body: e.body });
        else setErr({ status: 0, body: String(e) });
      });
    return () => {
      alive = false;
    };
  }, [experimentKey]);

  if (err) {
    if (err.status === 404) {
      return (
        <div className="ab-gray-card">
          <strong>实验 {experimentKey} 未找到</strong>
          <p className="muted">可能尚未启动或 key 拼写错误</p>
        </div>
      );
    }
    return <p className="error">A/B 加载失败：{err.status} {err.body}</p>;
  }
  if (data === null) {
    return <p className="muted">加载 A/B 臂…</p>;
  }
  return (
    <div className="ab-view">
      <p className="muted">
        实验 <strong>{data.experiment}</strong> · 数据时间 {data.fetched_at}
      </p>
      <table className="ab-table">
        <thead>
          <tr>
            <th>arm</th>
            <th>calls</th>
            <th>p95 (ms)</th>
            <th>cost (minor)</th>
            <th>未定价</th>
          </tr>
        </thead>
        <tbody>
          {data.arms.length === 0 ? (
            <tr><td colSpan={5} className="muted">无臂数据</td></tr>
          ) : null}
          {data.arms.map((a) => (
            <tr key={a.arm}>
              <td>{a.arm}</td>
              <td>{a.calls}</td>
              <td>{a.latency_p95_ms}</td>
              <td>{a.cost_total_minor} {a.currency}</td>
              <td>{a.unpriced_count}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="muted ab-note">注：LLM-judge 评分未启用，不展示 judge_score 列（F8 缺口）</p>
    </div>
  );
}
