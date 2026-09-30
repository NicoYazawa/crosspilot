// 订单详情占位（Step 5 接入 ReplayView + CostView）。
import { useParams } from 'react-router-dom';

export function OrderDetailView() {
  const { runId } = useParams<{ runId: string }>();
  return (
    <section>
      <h1>订单详情</h1>
      <p>runId: {runId ?? '(空)'}</p>
      <p>订单详情占位 — Step 5 接入 ReplayView + CostView</p>
    </section>
  );
}
