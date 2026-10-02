// 选购视图（Step 6 接入 useChat + useOptimistic + SSE 客户端）。
//
// 数据流：
//   1. 用户输入 → send(text)
//   2. useOptimistic 立即把 user message 上屏（< 16ms 上屏延迟，G1）
//   3. fetch POST /commerce/ag-ui/run → run_id
//   4. SSE 客户端订阅 /commerce/ag-ui/runs/{id}/events
//   5. onEvent 把 model_turn / a2ui 落到 committed，渲染

import { useState, type FormEvent } from 'react';
import { useChat } from './useChat';
import { A2UIRenderer } from '@/features/a2ui/Renderer';
import { recordRun } from '@/lib/api/history';

// 构建期注入的鉴权配置。会话标识存在 sessionStorage 里，让同一标签页刷新后
// 仍是同一个会话——否则每次刷新都换一个会话，agent 会忘记刚才聊到哪。
const AUTH_TOKEN = import.meta.env.VITE_AUTH_TOKEN ?? '';
const SESSION_ID = sessionStorage.getItem('crosspilot.session') ?? newSessionId();

function newSessionId(): string {
  const id = `sess_${Math.random().toString(36).slice(2, 12)}`;
  sessionStorage.setItem('crosspilot.session', id);
  return id;
}

export function ShopView() {
  const [query, setQuery] = useState('');
  const { messages, a2uiMessages, isPending, error, send, stop } = useChat({
    baseUrl: '',
    // 令牌由部署方通过 VITE_AUTH_TOKEN 注入（生产由身份服务签发）。
    // 为空表示本部署未启用鉴权，后端会补 demo 身份。
    authToken: AUTH_TOKEN,
    sessionId: SESSION_ID,
    onMetric: (name, value) => {
      window.console.debug(`[metric] ${name}=${value.toFixed(2)}ms`);
    },
    // 把这一轮记进本地历史，订单页 / 历史页靠它回看。
    //
    // 写失败只记一条 console.error：IndexedDB 是本地缓存，不是事实源。
    // 让写历史失败把一轮对话也搞失败，是把辅助功能当成了主链路。
    onRunSubmitted: (runId, query) => {
      void recordRun({ runId, lastEventAt: Date.now(), query }).catch((e: unknown) => {
        window.console.error('[history] 记录本轮失败（不影响对话）', e);
      });
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
