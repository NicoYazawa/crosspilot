// 三件套面板测试 — G5 验收。
//
// 覆盖：
//   - ReplayView：空 / 正常 / API 错误 / REDACTED 徽标
//   - DiffView：baseline===against / 正常 items
//   - CostView：unpriced banner / 正常数据
//   - ABView：404 灰卡 / 正常 arms
//   - MetricsGrayCard：503 灰卡 / 正常 snapshot

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { createRoot, type Root } from 'react-dom/client';
import { flushSync } from 'react-dom';
import { ReplayView } from './ReplayView';
import { DiffView } from './DiffView';
import { CostView } from './CostView';
import { ABView } from './ABView';
import { MetricsGrayCard } from './MetricsGrayCard';

let container: HTMLDivElement;
let root: Root;
let originalFetch: typeof fetch;

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function mount(node: React.ReactNode): void {
  flushSync(() => root.render(node));
}

beforeEach(() => {
  container = document.createElement('div');
  document.body.appendChild(container);
  root = createRoot(container);
  originalFetch = globalThis.fetch;
});

afterEach(() => {
  root.unmount();
  container.remove();
  globalThis.fetch = originalFetch;
});

describe('ReplayView', () => {
  it('空列表显示「无事件记录」', async () => {
    globalThis.fetch = vi.fn(async () => jsonResponse({ run_id: 'r1', events: [], total_seq: 0, has_more: false })) as unknown as typeof fetch;
    mount(<ReplayView runId="r1" />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('无事件记录');
    });
  });

  it('正常事件渲染时间线 + kind tag', async () => {
    const events = [
      { event_id: 'r1:0:1', run_id: 'r1', seq: 0, kind: 'run_start', created_at: '2026-09-30T00:00:00Z' },
      { event_id: 'r1:1:1', run_id: 'r1', seq: 1, kind: 'run_finished', created_at: '2026-09-30T00:00:01Z' },
    ];
    globalThis.fetch = vi.fn(async () => jsonResponse({ run_id: 'r1', events, total_seq: 2, has_more: false })) as unknown as typeof fetch;
    mount(<ReplayView runId="r1" />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('run_start');
      expect(container.textContent).toContain('run_finished');
    });
  });

  it('含 REDACTED 字样的 payload 旁显 ⛔ 徽标', async () => {
    const events = [
      { event_id: 'r1:0:1', run_id: 'r1', seq: 0, kind: 'model_turn', payload: { content: '<REDACTED>' }, created_at: '2026-09-30T00:00:00Z' },
    ];
    globalThis.fetch = vi.fn(async () => jsonResponse({ run_id: 'r1', events, total_seq: 1, has_more: false })) as unknown as typeof fetch;
    mount(<ReplayView runId="r1" />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('⛔ REDACTED');
    });
  });

  it('API 错误显示 error', async () => {
    globalThis.fetch = vi.fn(async () => jsonResponse({ error: 'internal' }, 500)) as unknown as typeof fetch;
    mount(<ReplayView runId="r1" />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('回放失败');
    });
  });
});

describe('DiffView', () => {
  it('baseline===against 拒绝', () => {
    mount(<DiffView baselineRunId="r1" againstRunId="r1" />);
    expect(container.textContent).toContain('不能相同');
  });

  it('正常 diff 渲染 badges + items', async () => {
    const body = {
      baseline_run_id: 'r1',
      against_run_id: 'r2',
      added: 1,
      removed: 0,
      changed: 1,
      unchanged: 5,
      items: [
        { seq: 3, kind: 'added', baseline: null, against: { x: 1 }, reason: 'extra in against' },
        { seq: 5, kind: 'changed', baseline: { v: 'a' }, against: { v: 'b' }, reason: 'value changed' },
      ],
    };
    globalThis.fetch = vi.fn(async () => jsonResponse(body)) as unknown as typeof fetch;
    mount(<DiffView baselineRunId="r1" againstRunId="r2" />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('added: 1');
      expect(container.textContent).toContain('extra in against');
    });
  });
});

describe('CostView', () => {
  it('unpriced_count > 0 → 显式 ⚠️ banner', async () => {
    globalThis.fetch = vi.fn(async () => jsonResponse({
      run_id: 'r1', total_cost_minor: 100, currency: 'CNY',
      unpriced_count: 3, total_calls: 10,
      tokens_in: 1000, tokens_out: 500, tokens_cached: 200, tokens_reasoning: 0,
      by_provider: [],
    })) as unknown as typeof fetch;
    mount(<CostView runId="r1" />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('未定价调用');
    });
  });

  it('unpriced_count = 0 不显示 banner', async () => {
    globalThis.fetch = vi.fn(async () => jsonResponse({
      run_id: 'r1', total_cost_minor: 100, currency: 'CNY',
      unpriced_count: 0, total_calls: 10,
      tokens_in: 1000, tokens_out: 500, tokens_cached: 200, tokens_reasoning: 0,
      by_provider: [{ provider: 'p', model: 'm', calls: 10, cost_minor: 100, currency: 'CNY', unpriced_count: 0, tokens_in: 1000, tokens_out: 500 }],
    })) as unknown as typeof fetch;
    mount(<CostView runId="r1" />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('总成本');
      expect(container.textContent).not.toContain('未定价调用');
    });
  });
});

describe('ABView', () => {
  it('404 → 「实验未找到」灰卡', async () => {
    globalThis.fetch = vi.fn(async () => jsonResponse({ error: 'experiment not found' }, 404)) as unknown as typeof fetch;
    mount(<ABView experimentKey="missing-exp" />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('实验 missing-exp 未找到');
    });
  });

  it('正常 arms 渲染表格 + LLM-judge 备注', async () => {
    globalThis.fetch = vi.fn(async () => jsonResponse({
      experiment: 'exp1',
      fetched_at: '2026-09-30T00:00:00Z',
      arms: [
        { arm: 'control', calls: 50, latency_p95_ms: 1200, cost_total_minor: 500, currency: 'CNY', unpriced_count: 0 },
        { arm: 'treatment', calls: 48, latency_p95_ms: 1100, cost_total_minor: 480, currency: 'CNY', unpriced_count: 0 },
      ],
    })) as unknown as typeof fetch;
    mount(<ABView experimentKey="exp1" />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('control');
      expect(container.textContent).toContain('treatment');
      expect(container.textContent).toContain('LLM-judge');
    });
  });
});

describe('MetricsGrayCard', () => {
  it('503 → 「未启用」灰卡', async () => {
    globalThis.fetch = vi.fn(async () => jsonResponse({ error: 'metrics 未挂载（容器未装配）' }, 503)) as unknown as typeof fetch;
    mount(<MetricsGrayCard />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('metrics 未启用');
    });
  });

  it('正常 snapshot 渲染计数', async () => {
    globalThis.fetch = vi.fn(async () => jsonResponse({
      queue_depth: 2, dropped_total: 0, emit_total: 100, emit_errors_total: 0, redact_errors: 0,
    })) as unknown as typeof fetch;
    mount(<MetricsGrayCard />);
    await vi.waitFor(() => {
      expect(container.textContent).toContain('queue_depth: 2');
      expect(container.textContent).toContain('emit_total: 100');
    });
  });
});
