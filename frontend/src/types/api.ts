// 后端 observability 路由的 TS 类型（与 P5 后端 snake_case JSON tag 一一对应）。
// 来源：
//   - CostSummary        internal/application/observability/cost.go:11
//   - ProviderBreakdown  internal/application/observability/cost.go:27
//   - DiffItem / DiffResult  internal/application/observability/diff.go
//   - ArmSummary         internal/application/observability/ports.go:75
//   - MetricsSnapshot    internal/agent/observability/metrics.go:85
//
// 字段命名严格走 snake_case；前端消费时由 Zod schema 二次校验（见 @/lib/sse/...）。

export interface ProviderBreakdown {
  provider: string;
  model: string;
  calls: number;
  cost_minor: number;
  currency: string;
  unpriced_count: number;
  tokens_in: number;
  tokens_out: number;
}

export interface CostSummary {
  run_id: string;
  total_cost_minor: number;
  currency: string;
  unpriced_count: number;
  total_calls: number;
  tokens_in: number;
  tokens_out: number;
  tokens_cached: number;
  tokens_reasoning: number;
  by_provider: ProviderBreakdown[];
}

export interface CostEvent {
  event_id: string;
  run_id: string;
  provider: string;
  model: string;
  tokens_in: number;
  tokens_out: number;
  tokens_cached: number;
  tokens_reasoning: number;
  cost_minor: number;
  currency: string;
  unpriced: boolean;
}

export type DiffKind = 'added' | 'removed' | 'changed';

export interface DiffItem {
  seq: number;
  kind: DiffKind;
  baseline: unknown | null;
  against: unknown | null;
  reason: string;
}

export interface DiffResult {
  baseline_run_id: string;
  against_run_id: string;
  items: DiffItem[];
  added: number;
  removed: number;
  changed: number;
  unchanged: number;
}

export interface ArmSummary {
  arm: string;
  calls: number;
  latency_p95_ms: number;
  cost_total_minor: number;
  currency: string;
  unpriced_count: number;
}

export interface MetricsSnapshot {
  queue_depth: number;
  dropped_total: number;
  emit_total: number;
  emit_errors_total: number;
  redact_errors: number;
}

// /observability/metrics 在容器未装配时返回 503 + body = { error: '...' }
export interface MetricsUnavailableResponse {
  error: string;
}
