// AG-UI 协议类型（与后端 runevent + SubmitRequest 一一对应）。
// 来源：
//   - runevent.Event  internal/agent/runevent/runevent.go:45
//   - SubmitRequest   internal/presentation/agui/handler.go:48

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

// 触发 tail 轮询停止的 kind
export const TERMINAL_KINDS: ReadonlySet<RunEventKind> = new Set([
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

export interface SubmitRequest {
  run_id?: string;
  buyer_id: string;
  session_id: string;
  query: string;
  agent?: string;
}

export interface SubmitResponse {
  run_id: string;
  events: RunEvent[];
}

export interface RunMeta {
  run_id: string;
  last_seq: number;
}
