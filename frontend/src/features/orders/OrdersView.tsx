// 订单列表视图。
//
// 数据源：前端 IndexedDB runHistory（P7 后端补 GET /agui/runs 后切换）。
// 每行点击进入 /orders/:runId 看详情（Step 5 接入 ReplayView + CostView）。
//
// 排序：按 lastEventAt 倒序，最近的在前。

import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import type { RunHistoryItem } from '@/lib/api/history';
import { listRuns } from '@/lib/api/history';

export function OrdersView() {
  const [items, setItems] = useState<RunHistoryItem[] | null>(null);

  useEffect(() => {
    let alive = true;
    listRuns()
      .then((runs) => {
        if (alive) setItems(runs);
      })
      .catch(() => {
        if (alive) setItems([]);
      });
    return () => {
      alive = false;
    };
  }, []);

  if (items === null) {
    return (
      <section>
        <h1>订单</h1>
        <p>加载中…</p>
      </section>
    );
  }

  if (items.length === 0) {
    return (
      <section>
        <h1>订单</h1>
        <p>暂无订单 — 回到<Link to="/"> 选购 </Link>页发起一次询问。</p>
      </section>
    );
  }

  return (
    <section>
      <h1>订单</h1>
      <table className="orders-table">
        <thead>
          <tr>
            <th>runId</th>
            <th>最近事件</th>
            <th>查询</th>
          </tr>
        </thead>
        <tbody>
          {items.map((it) => (
            <tr key={it.runId}>
              <td>
                <Link to={`/orders/${encodeURIComponent(it.runId)}`}>{it.runId}</Link>
              </td>
              <td>{new Date(it.lastEventAt).toLocaleString('zh-CN')}</td>
              <td>{it.query ?? '—'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
