// Playwright E2E —— 下单全链路（G7 验收）。
//
// 全部 mock 后端；不依赖真实模型。

import { test, expect } from '@playwright/test';
import { buildAguiScript, mockAguiRuns, mockObservability } from './fixtures';

const RUN_ID = 'run-e2e-001';

test.describe('下单全链路', () => {
  test('选购 → Agent 回复 → A2UI 渲染 → 订单详情', async ({ page }) => {
    await mockAguiRuns(page, buildAguiScript(RUN_ID));
    await mockObservability(page, RUN_ID);

    await page.goto('/');
    await expect(page.getByRole('heading', { name: '选购' })).toBeVisible();

    const input = page.getByLabel('查询');
    await input.fill('找一款防水登山包');

    const t0 = Date.now();
    await page.getByRole('button', { name: '发送' }).click();
    // 等待 React 19 startTransition flush 后 DOM 更新
    await expect(page.locator('.chat-msg.chat-user')).toBeVisible({ timeout: 10000 });
    await expect(page.locator('.chat-msg.chat-user')).toContainText('找一款防水登山包');
    const t1 = Date.now();
    expect(t1 - t0).toBeLessThan(2000); // 整体上屏链路 2s 预算（含 mock SSE 解析）

    // A2UI ProductCard 渲染
    await expect(page.locator('.a2ui-product-card')).toContainText('防水登山包', { timeout: 5000 });

    // 输入框清空
    await expect(input).toHaveValue('');
  });

  test('订单详情页加载 Replay + Cost + Metrics 灰卡', async ({ page }) => {
    await mockObservability(page, RUN_ID);
    await page.goto(`/#/orders/${RUN_ID}`);
    await expect(page.getByRole('heading', { name: '订单详情' })).toBeVisible();
    // CostView 加载
    await expect(page.locator('.cost-view')).toContainText('总成本');
    // MetricsGrayCard 503 灰卡
    await expect(page.locator('.metrics-gray-card')).toContainText('metrics 未启用', { timeout: 5000 });
  });
});
