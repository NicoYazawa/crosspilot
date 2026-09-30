// SSE 客户端共享类型。
//
// 与后端 internal/presentation/agui/replay.go + runevent.go 严格对齐：
//   - Cursor = "{runId}:{seq}"（SSE Last-Event-ID + query cursor 两种形态）
//   - Kind 枚举与 TERMINAL_KINDS 与 internal/agent/runevent/runevent.go:29-39 对齐
//   - Event JSON tag 与 internal/agent/runevent/runevent.go:45-53 对齐
//
// 设计要点：
//   - TERMINAL_KINDS 前端独有（包含 confirm_decided），因为用户决议是前端订阅终点。
//   - SseError 用判别联合，每种 kind 携带不同字段——便于 UI 区分渲染（重连 / 灰卡）。

export type RunEventKind =
  | 'run_start'
  | 'model_turn'
  | 'tool_call'
  | 'tool_result'
  | 'run_finished'
  | 'run_error'
  | 'server_restart'
  | 'a2ui'
  | 'heartbeat'
  | 'confirm_decided';

// 前端订阅终点。订阅见到任一 kind 即停 SSE + 停 polling fallback。
// run_finished / run_error 是服务端终止点；confirm_decided 是用户决议点。
export const TERMINAL_KINDS: ReadonlySet<RunEventKind> = new Set<RunEventKind>([
  'run_finished',
  'run_error',
  'confirm_decided',
]);

export interface RunEvent {
  event_id: string;
  run_id: string;
  seq: number;
  kind: RunEventKind;
  agent?: string;
  payload?: unknown;
  created_at: string;
}

export interface Cursor {
  runId: string;
  seq: number;
}

// SseError 用判别联合，便于 UI 渲染策略分流：
//   - gap        → 重连请求被拒；UI 触发「重置后从头订阅」流程
//   - cross-run  → 客户端 bug；UI 灰卡提示
//   - cursor-invalid → 协议层错误；UI 重置 cursor
//   - http       → 服务端 4xx/5xx；UI 弹提示横幅
//   - network    → 物理断流；UI 不显式提示，由 SSE 重连机制兜底
export type SseError =
  | { kind: 'gap'; expected: number; actual: number }
  | { kind: 'cross-run'; cursorRunId: string; targetRunId: string }
  | { kind: 'cursor-invalid'; raw: string }
  | { kind: 'http'; status: number; body: string }
  | { kind: 'network'; cause: unknown };

export interface CursorFormat {
  toHeader(): string;
  toQuery(): string;
}

export interface PollMeta {
  intervalMs: number;
  onTerminal(meta: RunMeta): void;
}

export interface RunMeta {
  run_id: string;
  last_seq: number;
}

export interface SseClientOptions {
  baseUrl: string;
  runId: string;
  initialCursor?: Cursor | null;
  hmacSecret?: string;
  onEvent: (ev: RunEvent) => void;
  onError: (err: SseError) => void;
  signal?: AbortSignal;
  pollMeta?: PollMeta;
}

// SSE 报文单条解析后的中间形态（拆分 event / data / id 行）。
export interface ParsedSseFrame {
  id: string;
  event: string;
  data: unknown;
}
