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

// SubmitRequest 是 POST /commerce/ag-ui/run 的请求体。
//
// 没有 buyer_id / session_id：后端的对应字段标了 json:"-"，身份只从令牌
// （Authorization: Bearer）与 X-Session-ID 解析。放在这里会让调用方以为
// 客户端能指定身份——那正是被堵掉的越权路径。
export interface SubmitRequest {
  run_id?: string;
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
