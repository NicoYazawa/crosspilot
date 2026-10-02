// 选购流程核心 hook：useOptimistic + useTransition + SSE 订阅。
//
// 设计要点：
//   - 用户消息走 useOptimistic：发送时立即上屏，< 16ms 上屏延迟（G1 验收）。
//     注意「立即」的实现方式不是「调一次 addOptimistic」，而是让 addOptimistic
//     与整轮提交待在同一个 transition 里——否则乐观态会当场被撤销，详见 send()。
//   - 服务端消息（model_turn / a2ui）走 SSE 流，不走乐观（避免与真实事件冲突）
//   - 提交时机：fetch POST /commerce/ag-ui/run 返回后，把 optimistic 标记为已提交
//     （仍是 useOptimistic 的特性：transition 结束时回到 committed）
//   - run_id 由前端先放进请求体（后端复用），响应回显后以回显值为准；这样
//     「停止」在 POST 未返回时也能指名取消正在跑的推理，详见 stop()
//   - 鉴权：Authorization: Bearer <JWT>；会话走 X-Session-ID

import { useCallback, useOptimistic, useRef, useState, useTransition } from 'react';
import type { RunEvent } from '@/types/agui';
import { RecoveringSseClient } from '@/lib/sse/client';
import type { SseError } from '@/lib/sse/types';
import { buildAuthHeaders } from '@/lib/auth';

export interface ChatMessage {
  id: string;
  role: 'user' | 'agent';
  content: string;
  pending: boolean;
}

export interface UseChatOptions {
  baseUrl: string;
  // authToken 是后端签发的 JWT；空表示本部署未启用鉴权。
  authToken?: string;
  // sessionId 走 X-Session-ID，与令牌分离（同一买家可开多个会话）。
  sessionId?: string;
  onMetric?: (name: string, value: number, tags?: Record<string, string>) => void;
  // onRunSubmitted 在 POST 成功、run_id 确定之后回调一次。
  //
  // 存在的理由：后端没有「列出我的 run」的接口，订单页与历史页只能读前端自己
  // 记下的 run_id。这个回调就是把「已提交的一轮」写进本地历史的唯一时机——
  // 少了它，那两页永远是空表（此前 recordRun 就是这么变成死代码的）。
  //
  // 回调本身不应抛错；调用方负责把它变成「写失败也不影响对话」。
  onRunSubmitted?: (runId: string, query: string) => void;
}

export interface UseChatResult {
  messages: ChatMessage[];
  a2uiMessages: unknown[];
  isPending: boolean;
  error: string | null;
  send: (text: string) => Promise<void>;
  stop: () => void;
}

// SubmitRequest 与后端 agui.SubmitRequest 的 JSON 形态对齐。
//
// 没有 buyer_id / session_id：后端把它们标了 json:"-"，身份一律取自令牌与
// X-Session-ID。在请求体里带上它们只会误导后来的人以为客户端能决定身份。
//
// run_id 反过来**由客户端先给**：后端声明它是 `run_id,omitempty`，为空时自己
// 生成一个（handler.go 的「解析请求体拿到 run_id（生成或复用）」）。客户端自带
// id 是为了让「停止」按钮在 POST 还在跑的时候就能指名取消——那时响应还没回来，
// 拿不到服务端回显的 id，见 stop()。响应回来之后仍以回显值为准（submitRun）。
interface SubmitRequest {
  run_id: string;
  query: string;
  agent: string;
}

interface SubmitResponse {
  run_id: string;
}

const LATENCY_METRIC = 'crosspilot.optimistic.add_latency_ms';

// runSeq 让同一毫秒内生成的多个 run_id 不撞车。
//
// id 由 `run_<毫秒时间戳>` 构成（与后端 newRunID 的形式一致，便于两端日志
// 按时间排序）。单靠毫秒在真实使用里其实够用——提交期间输入框是禁用的，
// 不会有第二个请求并发——但「够用」依赖 UI 的禁用状态，而 id 唯一性不该
// 依赖别处的 UI 约束。
let runSeq = 0;

function newRunID(): string {
  runSeq += 1;
  return `run_${Date.now()}${runSeq}`;
}

export function useChat(opts: UseChatOptions): UseChatResult {
  const [committed, setCommitted] = useState<ChatMessage[]>([]);
  const [a2uiMessages, setA2uiMessages] = useState<unknown[]>([]);
  const [optimistic, addOptimistic] = useOptimistic<ChatMessage[], ChatMessage>(
    committed,
    (curr, next) => [...curr, next],
  );
  const [isPending, startTransition] = useTransition();
  const [error, setError] = useState<string | null>(null);
  const [client, setClient] = useState<RecoveringSseClient | null>(null);
  // runIdRef 是当前这一轮请求的 run_id。用 ref 而不是 state：stop() 要在
  // POST 还挂着的时候就读到它，而那一刻组件还没拿到任何新 state。
  const runIdRef = useRef<string | null>(null);
  // cancelRequestedRef 记住「这次失败是用户自己按的停止」。
  //
  // 取消会让正在跑的 run 以 ctx 取消收场，POST 随之回 500 submit_failed——
  // 分类上它确实是「服务端故障之外的失败」，但对按了停止的人来说这不是
  // 故障，是他要的结果。不区分的话，一次主动停止会得到一个红色错误横幅，
  // 而且乐观消息在 transition 结束时被撤销、用户自己那条消息当场上屏又消失。
  const cancelRequestedRef = useRef(false);

  const onEvent = useCallback((ev: RunEvent): void => {
    if (ev.kind === 'model_turn') {
      const payload = (ev.payload ?? {}) as { content?: string };
      const text = typeof payload.content === 'string' ? payload.content : '';
      if (text !== '') {
        setCommitted((prev) => [...prev, { id: ev.event_id, role: 'agent', content: text, pending: false }]);
      }
    }
    if (ev.kind === 'a2ui') {
      // 同时记录给 Renderer（原始 payload）
      setA2uiMessages((prev) => [...prev, ev.payload]);
      // 并在对话流里留一条文本摘要
      const payload = (ev.payload ?? {}) as { action?: string };
      setCommitted((prev) => [...prev, {
        id: ev.event_id,
        role: 'agent',
        content: `[A2UI ${payload.action ?? 'message'}]`,
        pending: false,
      }]);
    }
  }, []);

  const onError = useCallback((err: SseError): void => {
    window.console.error('[sse error]', err);
    setError(`${err.kind}${err.kind === 'gap' ? ` expected=${err.expected} actual=${err.actual}` : ''}`);
  }, []);

  // submitRun 是一次提交的全部动作：POST 拿 run_id → 把乐观消息转为已提交 →
  // 启动 SSE 订阅。单独抽出来是为了让 send 能把它放进与 addOptimistic 同一个
  // transition 里 await——原因见 send 里的说明。
  const submitRun = useCallback(
    async (sentRunID: string, text: string, msg: ChatMessage): Promise<void> => {
      let runID = sentRunID;

      // 用户按了停止，这一轮是他主动中断的：留下他发的那条消息并说明结果，
      // 不要把它当成一次服务端故障。落到这里的两条路——取消后 POST 回
      // 500 submit_failed，以及连接被掐断——对按了停止的人都只是同一件事。
      const settleAsCancelled = (): void => {
        setCommitted((prev) => [...prev, { ...msg, pending: false }]);
        setError('已停止：本轮推理已中断。');
      };

      try {
        const res = await fetch(`${opts.baseUrl}/commerce/ag-ui/run`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            ...buildAuthHeaders(opts.authToken, opts.sessionId),
          },
          body: JSON.stringify({
            run_id: sentRunID,
            query: text,
            agent: 'shopping',
          } satisfies SubmitRequest),
        });
        if (!res.ok) {
          if (cancelRequestedRef.current) {
            settleAsCancelled();
            return;
          }
          setError(await describeSubmitFailure(res));
          return;
        }
        // run_id 以服务端回显的为准：它才是这次 run 的标识（请求里为空时由它
        // 生成）。生产路径上它与我们发出去的那个相同，所以 POST 还在跑的时候
        // 就按发出去的那个取消也取消得对；这里只是不让前端把自己的假设当成事实
        // ——后端哪天改了生成规则，订阅和取消不该跟着一起错。
        const data = (await res.json()) as SubmitResponse;
        if (typeof data.run_id === 'string' && data.run_id !== '') {
          runID = data.run_id;
          runIdRef.current = runID;
        }
      } catch (e) {
        if (cancelRequestedRef.current) {
          settleAsCancelled();
          return;
        }
        setError(e instanceof Error ? e.message : String(e));
        return;
      }

      // 把乐观消息标记为已提交
      setCommitted((prev) => [...prev, { ...msg, pending: false }]);

      // 记下这一轮，供订单页 / 历史页回看。放在 run_id 确定之后、SSE 之前：
      // 此刻这次 run 已经在服务端存在了，与订阅是否能接上无关。
      //
      // 用 try 包住而不是让它冒出去：onRunSubmitted 是宿主的副作用（写
      // IndexedDB），写本地历史失败不该让一轮对话失败。
      try {
        opts.onRunSubmitted?.(runID, text);
      } catch (e) {
        console.error('[useChat] onRunSubmitted 抛错，已忽略', e);
      }

      // 启动 SSE 订阅
      const sseOpts: ConstructorParameters<typeof RecoveringSseClient>[0] = {
        baseUrl: opts.baseUrl,
        runId: runID,
        onEvent,
        onError,
      };
      if (opts.authToken !== undefined) sseOpts.authToken = opts.authToken;
      if (opts.sessionId !== undefined) sseOpts.sessionId = opts.sessionId;
      const c = new RecoveringSseClient(sseOpts);
      setClient(c);
      void c.start();
    },
    [opts, onEvent, onError],
  );

  const send = useCallback(
    (text: string): Promise<void> => {
      setError(null);
      const t0 = performance.now();
      const msg: ChatMessage = {
        id: `pending-${Date.now()}`,
        role: 'user',
        content: text,
        pending: true,
      };

      // run_id 由客户端先定：POST /run 是同步的（一次跑完整轮 ReAct 才返回），
      // 若等后端在响应里给 id，那 id 到手时这一轮早已跑完，「停止」就永远没有
      // 可取消的窗口。自带 id 后，POST 还在跑的时候 stop() 就能指名取消它。
      const runID = newRunID();
      runIdRef.current = runID;
      cancelRequestedRef.current = false;

      const submitted = submitRun(runID, text, msg);

      // 整轮提交必须与 addOptimistic 待在**同一个** transition 里。
      //
      // useOptimistic 的乐观态只在它所属的那个 transition 挂起期间存在。原来是
      // `startTransition(() => { addOptimistic(msg); })`：回调同步返回，transition
      // 在同一个 tick 内就结束，乐观态当即被撤销——用户消息要等到 POST 返回才出现。
      // 而真实链路的 POST 是「一次跑完整轮 ReAct」，实测 6–15s，于是按下发送之后
      // 要盯着空屏幕十几秒。同一个原因还让 isPending 立刻归 false：按钮显示不出
      // 「提交中…」，输入框也不会禁用。
      //
      // mock 套件里这三个症状都看不见——被 mock 的 POST 是瞬时返回的。
      // 真链路的对照断言在 tests/e2e/real-backend.spec.ts 第 1 段。
      startTransition(async () => {
        addOptimistic(msg);
        await submitted;
      });

      const t1 = performance.now();
      opts.onMetric?.(LATENCY_METRIC, t1 - t0);
      return submitted;
    },
    // addOptimistic 来自 useOptimistic，引用稳定；显式省略避免 exhaustive-deps 噪音。
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [opts, submitRun],
  );

  // stop 做两件事，缺一不可：关掉 SSE 订阅，以及**真的**让推理停下。
  //
  // 只关流是这里原本的行为，它有两个问题。一是订阅本来就没打开——SSE 是在
  // POST 返回之后才订阅的，而 POST 跑完整轮 ReAct 要 6–15s，「停止」唯一被
  // 点得到的时刻恰恰是流还没开的时候，于是 close() 作用在一个 null 上。
  // 二是即使关掉了流，也只是前端不再收事件：服务端的推理循环照跑，token
  // 照烧，账单上一个都不少——按钮说「停止」，实际什么也没停。
  //
  // 所以这里补上 POST /runs/{id}/cancel。后端接的是 Bridge.Cancel，它会
  // cancel 掉这次 run 的 ctx，一路传到工具与模型的 HTTP 请求（ports.go）。
  //
  // 取消请求本身不等结果：run 可能刚好在这条请求到达前结束了（回 404
  // run_not_active），那是正常竞态而不是错误，弹给用户只是噪音；真正的
  // 结果由这次 run 自己的事件说明（run_error / run_finished）。
  const stop = useCallback((): void => {
    cancelRequestedRef.current = true;
    const runID = runIdRef.current;
    if (runID !== null) {
      void fetch(`${opts.baseUrl}/commerce/ag-ui/runs/${encodeURIComponent(runID)}/cancel`, {
        method: 'POST',
        headers: buildAuthHeaders(opts.authToken, opts.sessionId),
      }).catch((e: unknown) => {
        // 网络层失败要说出来：它和「已结束」不是一回事，静默会让人以为取消
        // 生效了。
        window.console.error('[cancel 请求失败]', e);
      });
    }
    client?.close();
    setClient(null);
  }, [client, opts.baseUrl, opts.authToken, opts.sessionId]);

  return {
    messages: optimistic,
    a2uiMessages,
    isPending,
    error,
    send,
    stop,
  };
}

// 后端约定的错误码 → 给人看的话。后端只回 {"error": code}，不加任何细节
// （细节留在服务端日志里），所以「怎么解决」只能由这里补上。
const SUBMIT_ERROR_HINTS: Record<string, string> = {
  // 不写死某一家供应商：后端按 LLM_DEFAULT_PROVIDER 选中的那家取 key
  // （qwen → QWEN_API_KEY、deepseek → DEEPSEEK_API_KEY、minimax → MINIMAX_API_KEY）。
  // 提示里点名 QWEN_API_KEY 的话，换一家用的人会照着改错变量。
  model_unavailable:
    '后端还没接上模型：请在 .env 里填 LLM_DEFAULT_PROVIDER 所选那家的 API key' +
    '（如 DEEPSEEK_API_KEY / QWEN_API_KEY），再 docker compose up -d 重启后端。',
  missing_buyer: '请求未带买家身份：后端开启了鉴权但前端没有令牌。',
};

// describeSubmitFailure 把一次失败的提交翻译成可操作的提示。
//
// 之前只显示 `submit 500`——运维看到的是一个既不含原因、也不含下一步的状态码，
// 而真正的原因（没配模型 key）与它只在日志里。读一眼响应体里的错误码就能分清
// 「配置没做完」和「服务真的挂了」，这两件事的处置完全不同。
async function describeSubmitFailure(res: Response): Promise<string> {
  const code = await readErrorCode(res);
  if (code && SUBMIT_ERROR_HINTS[code]) {
    return `${SUBMIT_ERROR_HINTS[code]}（${res.status} ${code}）`;
  }
  if (code) {
    return `submit ${res.status}：${code}`;
  }
  return `submit ${res.status}`;
}

// readErrorCode 从响应体里取错误码；取不到就返回 null。
//
// 不假设响应体一定是 JSON：网关的 HTML 错误页、空 body 都可能出现在这里，
// 为此抛异常会把「显示不了错误提示」变成一个更难查的错误。
async function readErrorCode(res: Response): Promise<string | null> {
  try {
    const body = (await res.json()) as { error?: unknown };
    return typeof body.error === 'string' ? body.error : null;
  } catch {
    return null;
  }
}
