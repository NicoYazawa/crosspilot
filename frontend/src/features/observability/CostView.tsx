// CostView：成本面板（F4 unpriced 显式 + tokens 分类小图）。
//
// 数据：GET /observability/runs/{runId}/cost
// 视觉：cards 顶部统计；by_provider 表格；tokens 分类用 Recharts BarChart。

import { useEffect, useState } from 'react';
import { Bar, BarChart, CartesianGrid, Legend, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts';
import { ApiError, defaultApi } from '@/lib/api/client';
import { formatCost } from './money';
import type { CostSummary } from '@/types/api';

export interface CostViewProps {
  runId: string;
}

export function CostView({ runId }: CostViewProps) {
  const [data, setData] = useState<CostSummary | null>(null);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    let alive = true;
    defaultApi()
      .get<CostSummary>(`/observability/runs/${encodeURIComponent(runId)}/cost`)
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
  }, [runId]);

  if (err) {
    return <p className="error">成本加载失败：{err}</p>;
  }
  if (data === null) {
    return <p className="muted">加载成本…</p>;
  }
  const chartData = [
    { name: 'tokens', in: data.tokens_in, out: data.tokens_out, cached: data.tokens_cached, reasoning: data.tokens_reasoning },
  ];
  return (
    <div className="cost-view">
      {data.unpriced_count > 0 ? (
        <p className="cost-banner">
          ⚠️ 未定价调用：{data.unpriced_count}（总成本仅统计已定价部分）
        </p>
      ) : null}
      <div className="cost-cards">
        <div className="cost-card">
          <h3>总成本</h3>
          {/* 换算成货币单位显示：后端给的是 1e-6 元为单位的整数，
              裸打印会让人以为是元，差六个数量级。title 里保留原始值备查。 */}
          <p title={`${data.total_cost_minor} 最小单位（1e-6 ${data.currency}）`}>
            <strong>{formatCost(data.total_cost_minor, data.currency)}</strong>
          </p>
        </div>
        <div className="cost-card">
          <h3>总调用数</h3>
          <p><strong>{data.total_calls}</strong></p>
        </div>
        <div className="cost-card">
          <h3>未定价</h3>
          <p><strong>{data.unpriced_count}</strong></p>
        </div>
      </div>
      <div className="cost-chart">
        <h4>tokens 分类</h4>
        <ResponsiveContainer width="100%" height={200}>
          <BarChart data={chartData}>
            <CartesianGrid strokeDasharray="3 3" />
            <XAxis dataKey="name" />
            <YAxis />
            <Tooltip />
            <Legend />
            <Bar dataKey="in" fill="#1677ff" name="in" />
            <Bar dataKey="out" fill="#52c41a" name="out" />
            <Bar dataKey="cached" fill="#faad14" name="cached" />
            <Bar dataKey="reasoning" fill="#722ed1" name="reasoning" />
          </BarChart>
        </ResponsiveContainer>
      </div>
      <h4>按 provider / model</h4>
      <table className="cost-table">
        <thead>
          <tr>
            <th>provider</th>
            <th>model</th>
            <th>calls</th>
            <th>cost</th>
            <th>未定价</th>
          </tr>
        </thead>
        <tbody>
          {data.by_provider.length === 0 ? (
            <tr><td colSpan={5} className="muted">无 provider 数据</td></tr>
          ) : null}
          {data.by_provider.map((p) => (
            <tr key={`${p.provider}:${p.model}`}>
              <td>{p.provider}</td>
              <td>{p.model}</td>
              <td>{p.calls}</td>
              <td title={`${p.cost_minor} 最小单位（1e-6 ${p.currency}）`}>
                {formatCost(p.cost_minor, p.currency)}
              </td>
              <td>{p.unpriced_count}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
