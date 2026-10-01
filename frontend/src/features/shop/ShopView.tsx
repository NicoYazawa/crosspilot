// 选购视图（Step 6 接入 useChat + useOptimistic + SSE 客户端）。
//
// 数据流：
//   1. 用户输入 → send(text)
//   2. useOptimistic 立即把 user message 上屏（< 16ms 上屏延迟，G1）
//   3. fetch POST /agui/runs → run_id
//   4. SSE 客户端订阅 /agui/runs/{id}/events
//   5. onEvent 把 model_turn / a2ui 落到 committed，渲染

import { useState, type FormEvent } from 'react';
import { useChat } from './useChat';
import { A2UIRenderer } from '@/features/a2ui/Renderer';

export function ShopView() {
  const [query, setQuery] = useState('');
  const { messages, a2uiMessages, isPending, error, send, stop } = useChat({
    baseUrl: '',
    onMetric: (name, value) => {
      window.console.debug(`[metric] ${name}=${value.toFixed(2)}ms`);
    },
  });

  const onSubmit = (e: FormEvent<HTMLFormElement>): void => {
    e.preventDefault();
    if (query.trim() === '') return;
    void send(query.trim()).then(() => setQuery(''));
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
          disabled={isPending}
        />
        <button type="submit" disabled={isPending || query.trim() === ''}>
          {isPending ? '提交中…' : '发送'}
        </button>
        <button type="button" onClick={stop} disabled={!isPending}>停止</button>
      </form>

      {error ? <p className="error">{error}</p> : null}

      <div className="chat-history">
        {messages.length === 0 ? (
          <p className="muted">尚无消息</p>
        ) : (
          <ul>
            {messages.map((m) => (
              <li key={m.id} className={`chat-msg chat-${m.role}${m.pending ? ' pending' : ''}`}>
                <strong>{m.role === 'user' ? '我' : 'Agent'}:</strong> {m.content}
                {m.pending ? <span className="muted"> (发送中…)</span> : null}
              </li>
            ))}
          </ul>
        )}
      </div>

      <div className="a2ui-render">
        <A2UIRenderer
          messages={a2uiMessages}
          onError={(e) => window.console.error('[a2ui]', e.message)}
        />
      </div>
    </section>
  );
}
