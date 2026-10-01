// Playwright E2E fixture（Step 7 充实）。
//
// E2E 全部 mock 后端事件，不依赖真实模型。可重复、跑得快（< 10s）。

import type { Page, Route } from '@playwright/test';

export interface AguiScript {
  runId: string;
  events: Array<{
    kind: string;
    data: unknown;
  }>;
}

export function buildAguiScript(runId: string): AguiScript {
  return {
    runId,
    events: [
      { kind: 'run_start', data: { event_id: `${runId}:0`, run_id: runId, seq: 0, kind: 'run_start', agent: 'shopping', payload: {}, created_at: '2026-10-01T00:00:00Z' } },
      { kind: 'model_turn', data: { event_id: `${runId}:1`, run_id: runId, seq: 1, kind: 'model_turn', agent: 'shopping', payload: { content: '我帮你找几款' }, created_at: '2026-10-01T00:00:01Z' } },
      { kind: 'a2ui', data: { event_id: `${runId}:2`, run_id: runId, seq: 2, kind: 'a2ui', payload: {
        action: 'createSurface',
        catalogId: 'globex.local/shopping-v2',
        version: '0.9',
        components: [
          { id: 'root', type: 'Column', path: '/requirements', props: { children: ['card', 'subtotal', 'confirm'] } },
          { id: 'card', type: 'ProductCard', props: { title: '防水登山包', subtitle: '30L', price_minor: 39900, currency: 'CNY' } },
          { id: 'subtotal', type: 'SubtotalLine', props: { label: '小计', amount_minor: 39900, currency: 'CNY' } },
          { id: 'confirm', type: 'ConfirmationCard', props: { title: '订单已确认', order_id: 'demo-001' } },
        ],
      }, created_at: '2026-10-01T00:00:02Z' } },
      { kind: 'confirm_decided', data: { event_id: `${runId}:3`, run_id: runId, seq: 3, kind: 'confirm_decided', agent: 'user', payload: { confirmation_id: 'demo-001', approved: true }, created_at: '2026-10-01T00:00:03Z' } },
      { kind: 'run_finished', data: { event_id: `${runId}:4`, run_id: runId, seq: 4, kind: 'run_finished', agent: 'shopping', payload: {}, created_at: '2026-10-01T00:00:04Z' } },
    ],
  };
}

// 拼接 SSE 报文（id/event/data 三行 + 空行）。
function sseFrame(event: string, data: unknown, id?: string): string {
  const lines: string[] = [];
  if (id !== undefined) lines.push(`id: ${id}`);
  lines.push(`event: ${event}`);
  lines.push(`data: ${JSON.stringify(data)}`);
  return lines.join('\n') + '\n\n';
}

// mock /agui/runs POST + /agui/runs/{id}/events SSE + /agui/runs/{id} meta + /agui/runs/{id}/confirm
export async function mockAguiRuns(page: Page, script: AguiScript): Promise<void> {
  const runId = script.runId;

  await page.route('**/agui/runs', async (route: Route) => {
    const req = route.request();
    if (req.method() === 'POST') {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ run_id: runId }),
      });
    } else {
      await route.continue();
    }
  });

  await page.route(`**/agui/runs/${runId}/events`, async (route: Route) => {
    const body = script.events.map((e) => sseFrame(e.kind, e.data, `e-${e.kind}`)).join('');
    await route.fulfill({
      status: 200,
      contentType: 'text/event-stream',
      headers: {
        'Cache-Control': 'no-cache',
        Connection: 'keep-alive',
      },
      body,
    });
  });

  await page.route(`**/agui/runs/${runId}`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ run_id: runId, last_seq: 4 }),
    });
  });

  await page.route(`**/agui/runs/${runId}/confirm`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ run_id: runId, event: { event_id: `${runId}:5`, run_id: runId, seq: 5, kind: 'confirm_decided', agent: 'user', payload: {}, created_at: '2026-10-01T00:00:05Z' } }),
    });
  });
}

// mock observability 路由（订单详情用）。
export async function mockObservability(page: Page, runId: string): Promise<void> {
  await page.route(`**/observability/runs/${runId}/events**`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ run_id: runId, events: [], total_seq: 0, has_more: false }),
    });
  });
  await page.route(`**/observability/runs/${runId}/cost`, async (route: Route) => {
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({
        run_id: runId,
        total_cost_minor: 39900,
        currency: 'CNY',
        unpriced_count: 0,
        total_calls: 1,
        tokens_in: 100, tokens_out: 50, tokens_cached: 0, tokens_reasoning: 0,
        by_provider: [],
      }),
    });
  });
  await page.route(`**/observability/metrics`, async (route: Route) => {
    await route.fulfill({
      status: 503,
      contentType: 'application/json',
      body: JSON.stringify({ error: 'metrics 未挂载（容器未装配）' }),
    });
  });
}
