// 选购视图（Step 3 占位）。
//
// Step 6 接入 useOptimistic + SSE 流式回复 + A2UI 渲染（Step 4）。
// 当前仅展示查询输入框与占位提示，方便路由验证。

import { useState, type FormEvent } from 'react';

export function ShopView() {
  const [query, setQuery] = useState('');
  const [pending, setPending] = useState(false);

  const onSubmit = (e: FormEvent<HTMLFormElement>): void => {
    e.preventDefault();
    if (query.trim() === '') return;
    setPending(true);
    // Step 6：fetch POST /agui/runs + 启动 SSE；当前仅打印。
    window.console.log('[shop] submit query (placeholder):', query);
    setPending(false);
    setQuery('');
  };

  return (
    <section>
      <h1>选购</h1>
      <form onSubmit={onSubmit} className="shop-form">
        <input
          type="text"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder="描述你的需求，例如：找一款防水登山包"
          aria-label="查询"
          disabled={pending}
        />
        <button type="submit" disabled={pending || query.trim() === ''}>
          发送
        </button>
      </form>
      <p className="muted">Step 3 占位 — Step 6 接入 useOptimistic + SSE 流式</p>
    </section>
  );
}
