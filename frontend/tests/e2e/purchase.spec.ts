// 占位：Step 7 充实下单全链路 E2E。
import { test, expect } from '@playwright/test';

test('选购首页可访问（占位）', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: '选购' })).toBeVisible();
});
