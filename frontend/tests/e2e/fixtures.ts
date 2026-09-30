// Playwright fixture 占位（Step 7 充实）。
// E2E mock 全部后端事件；不依赖真实模型。
import type { Page } from '@playwright/test';

export interface AguiScript {
  events(runId: string): unknown[];
}

export async function mockAguiRuns(page: Page, script: AguiScript): Promise<void> {
  await page.route('**/agui/runs', async (route) => {
    if (route.request().method() === 'POST') {
      const body = JSON.parse(route.request().postData() ?? '{}') as { run_id?: string };
      const runId = body.run_id ?? `run_${Date.now()}`;
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ run_id: runId, events: script.events(runId) }),
      });
    } else {
      await route.continue();
    }
  });
}
