// 历史视图。
//
// 与订单视图共享数据源（IndexedDB runHistory）。区别：
//   - 订单视图强调「可追踪的当前订单」（按 runId 排序）
//   - 历史视图强调「全部回放」（按时间倒序，含 query 摘要）
//
// P7 后端补 GET /agui/runs（ListAll）后切换为后端数据源。

import { useEffect, useState } from 'react';
import { Link } from 'react-router-dom';
import type { RunHistoryItem } from '@/lib/api/history';
import { listRuns } from '@/lib/api/history';

export function HistoryView() {
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
        <h1>历史</h1>
        <p>加载中…</p>
      </section>
    );
  }
  if (items.length === 0) {
    return (
      <section>
        <h1>历史</h1>
        <p>暂无历史 — 回到<Link to="/"> 选购 </Link>页发起询问会自动写入</p>
      </section>
    );
  }

  return (
    <section>
      <h1>历史</h1>
      <table className="history-table">
        <thead>
          <tr>
            <th>runId</th>
            <th>查询</th>
            <th>时间</th>
          </tr>
        </thead>
        <tbody>
          {items.map((it) => (
            <tr key={it.runId}>
              <td>
                <Link to={`/orders/${encodeURIComponent(it.runId)}`}>{it.runId}</Link>
              </td>
              <td>{it.query ?? '—'}</td>
              <td>{new Date(it.lastEventAt).toLocaleString('zh-CN')}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  );
}
