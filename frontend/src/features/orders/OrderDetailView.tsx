// 订单详情视图。
//
// 数据源：
//   - GET /observability/runs/{runId}/events?from=0&limit=200 → ReplayView
//   - GET /observability/runs/{runId}/cost                    → CostView
//   - 可选 DiffView（baseline/against 由 URL ?against= 传入）
//
// Step 5 起把 ReplayView / CostView / DiffView 拆为独立子组件并接入图表。

import { useParams, useSearchParams } from 'react-router-dom';
import { ReplayView } from '@/features/observability/ReplayView';
import { CostView } from '@/features/observability/CostView';
import { DiffView } from '@/features/observability/DiffView';
import { MetricsGrayCard } from '@/features/observability/MetricsGrayCard';

export function OrderDetailView() {
  const { runId } = useParams<{ runId: string }>();
  const [searchParams] = useSearchParams();
  const against = searchParams.get('against') ?? '';

  if (!runId) {
    return (
      <section>
        <h1>订单详情</h1>
        <p>runId 缺失</p>
      </section>
    );
  }

  return (
    <section>
      <h1>订单详情</h1>
      <p>runId: {runId}</p>

      <section className="cost-section">
        <h2>成本</h2>
        <CostView runId={runId} />
      </section>

      {against !== '' && against !== runId ? (
        <section className="diff-section">
          <h2>对比 {against}</h2>
          <DiffView baselineRunId={runId} againstRunId={against} />
        </section>
      ) : null}

      <section className="replay-section">
        <h2>事件时间线</h2>
        <ReplayView runId={runId} />
      </section>

      <section className="metrics-section">
        <h2>观测通道</h2>
        <MetricsGrayCard />
      </section>
    </section>
  );
}
