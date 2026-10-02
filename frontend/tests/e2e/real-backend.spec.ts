// Playwright E2E —— 真实后端冒烟（不打任何 mock）。
//
// 与 purchase.spec.ts 的分工：
//   - purchase.spec.ts 全 mock，验证的是前端自身的渲染与状态机，快且确定；
//   - 本文件一分钱不 mock，验证的是 mock 永远测不到的那一段：后端真的会调
//     product_search_tool、真的会发 a2ui 报文、前端真的能把真实 payload 渲染成卡片。
//
// 默认跳过。它需要真实模型 key、会真的产生推理费用、且模型输出不完全确定，
// 因此不能进 CI 的常规回归（那会让「模型今天心情不好」变成一次红灯）。
// 跑它：
//
//   REAL_BACKEND=1 npx playwright test real-backend.spec.ts
//
// 前置：后端与前端都已起来（task deploy:up），且 .env 里已配好所选 provider 的 key。

import { test, expect } from '@playwright/test';

const ENABLED = process.env['REAL_BACKEND'] === '1';

// 真实模型一轮「检索 + 总结」实测约 5.5s。余量给到 90s：
// 这是冒烟测试，宁可慢也不要因为一次网络抖动红掉。
const MODEL_BUDGET_MS = 90_000;

test.describe('真实后端端到端（不打 mock）', () => {
  test.skip(!ENABLED, '需要真实后端与模型 key：REAL_BACKEND=1 npx playwright test real-backend.spec.ts');

  test('输入「登山包」→ 模型调 product_search_tool → SSE 发 a2ui → 渲染真实商品卡片', async ({
    page,
  }) => {
    const consoleErrors: string[] = [];
    page.on('console', (m) => {
      if (m.type() === 'error') consoleErrors.push(m.text());
    });
    page.on('pageerror', (e) => consoleErrors.push(`pageerror: ${e.message}`));

    // SSE 订阅发生在 POST 返回之后，所以先挂监听再点发送。
    const eventsResponse = page.waitForResponse(
      (r) => /\/commerce\/ag-ui\/runs\/[^/]+\/events/.test(r.url()),
      { timeout: MODEL_BUDGET_MS },
    );

    await page.goto('/');
    await expect(page.getByRole('heading', { name: '选购' })).toBeVisible();

    const input = page.getByLabel('查询');
    await input.fill('登山包');
    await page.getByRole('button', { name: '发送' }).click();

    // ---- 1. 用户消息靠乐观更新立即上屏 ----
    //
    // 预算 2000ms 与 purchase.spec.ts 相同。真链路上这一条特别有意义：后端是
    // 「一次 POST 跑完整轮 ReAct」，实测要 6–15s 才返回。如果乐观更新没生效，
    // 用户按下发送后要盯着空屏幕十几秒——而这正是 mock 套件永远测不出来的，
    // 因为被 mock 的 POST 是瞬时返回的。
    const t0 = Date.now();
    await expect(page.locator('.chat-msg.chat-user')).toContainText('登山包', { timeout: 2000 });
    console.log(`用户消息上屏：${Date.now() - t0}ms`);

    // ---- 2. SSE 线上的事实：报文形状 + 事件种类 ----
    //
    // 这一段断言的是**契约**，不是渲染结果：前端 client.ts 要求 data 里带
    // seq / kind / run_id（它靠 seq 做缺口检测 E2），所以线上必须真的是完整的
    // runevent.Event JSON，而不是只有 payload。渲染断言绿、这一段红，就说明
    // 「后端发的和前端读的不是一个形状」——这正是只有真链路才暴露得出来的缝。
    const wire = await (await eventsResponse).text();
    const frames = parseSseFrames(wire);
    expect(frames.length, 'SSE 一个帧都没有').toBeGreaterThan(0);

    const kinds = frames.map((f) => f.event);
    expect(kinds, '模型没有调用 product_search_tool').toContain('tool_call');
    expect(kinds, '工具没有返回结果').toContain('tool_result');
    expect(kinds, '后端没有下发 A2UI 报文').toContain('a2ui');
    expect(kinds, 'run 没有正常收尾').toContain('run_finished');

    for (const f of frames) {
      const ev = JSON.parse(f.data) as Record<string, unknown>;
      expect(typeof ev['seq'], `帧 ${f.event} 的 data 里没有 seq，前端会整条丢弃`).toBe('number');
      expect(ev['kind'], `帧 ${f.event} 的 data 里 kind 与 event 行不一致`).toBe(f.event);
      expect(typeof ev['run_id']).toBe('string');
    }
    // seq 必须从 0 连续，缺口会被前端判为 gap 并停流。
    expect(frames.map((f) => JSON.parse(f.data)['seq'] as number)).toEqual(
      frames.map((_, i) => i),
    );

    // ---- 3. 从线上报文推出「页面本该显示什么」----
    //
    // 只取**最后一个 surface**：模型一轮里可能检索多次（实测见过两次），而每次
    // 检索都以 createSurface 开头，前端遇到它会清空已有的 components。所以最终
    // 页面上的卡片，只由最后一个 createSurface 之后的那批报文决定。
    const a2uiPayloads = frames
      .filter((f) => f.event === 'a2ui')
      .map((f) => (JSON.parse(f.data) as { payload: A2UIMessage }).payload);

    const lastSurfaceAt = a2uiPayloads.reduce(
      (acc, m, i) => (m.action === 'createSurface' ? i : acc),
      0,
    );
    const expected = a2uiPayloads
      .slice(lastSurfaceAt)
      .filter((m): m is UpdateComponentsMessage => m.action === 'updateComponents')
      .flatMap((m) => m.components)
      .filter((c) => c.type === 'ProductCard')
      .map((c) => ({
        id: c.id,
        title: String(c.props?.['title'] ?? ''),
        productId: String(c.props?.['product_id'] ?? ''),
        price: Number(c.props?.['price_major']),
        currency: String(c.props?.['currency'] ?? ''),
      }));

    expect(expected.length, '线上一条 ProductCard 都没有').toBeGreaterThan(0);
    console.log(`线上卡片：${expected.map((e) => `${e.title}@${e.price}${e.currency}`).join(' | ')}`);

    // 线上每条都必须带得动价格。先单独判一次，是因为「缺字段」与「字段有了但没显示」
    // 是两侧各自的缺陷，混在一个断言里会让失败信息指向错误的一侧。
    expect(
      expected.filter((e) => !Number.isFinite(e.price)).map((e) => e.title),
      '线上这些卡片的 price_major 不是数字',
    ).toEqual([]);

    // ---- 4. 渲染结果必须逐张等于线上报文 ----
    //
    // 这一条不做「大概相关」这类主观判断，只问一个能用证据回答的问题：
    // **放进去的 N 张卡，是不是原样显示出来了 N 张？**
    // 它同时钉住两类真出过的问题：契约字段名漂移（ProductCard 曾读 price_minor，
    // 而生产侧发的是 price_major，卡片于是一律不显示价格），以及组件被正确校验、
    // 却因为容器用法不对而一张都不显示（见 a2ui_search.go 里关于 List 的说明）。
    const cards = page.locator('.a2ui-product-card');
    await expect(cards).toHaveCount(expected.length, { timeout: MODEL_BUDGET_MS });

    // DOM 实际拿到的字段。与上面「线上卡片」对照，能一眼看出差异出在哪一侧。
    console.log(
      `DOM 卡片：${JSON.stringify(
        await cards.evaluateAll((els) =>
          els.map((el) => ({
            id: el.getAttribute('data-component-id'),
            productId: el.getAttribute('data-product-id'),
            text: (el as HTMLElement).innerText.replace(/\n/g, ' '),
          })),
        ),
      )}`,
    );

    for (const [i, want] of expected.entries()) {
      const card = cards.nth(i);
      await expect(card).toHaveAttribute('data-component-id', want.id);
      await expect(card).toHaveAttribute('data-product-id', want.productId);
      await expect(card.locator('h3')).toHaveText(want.title);
      // 价格与币种必须真的渲染出来：ProductCard 在 price_major 取不到数字时
      // 整段省略价格——那是一种「卡片看起来没坏，只是没价」的静默失败。
      await expect(card, `第 ${i} 张卡没渲染价格`).toContainText(
        `${want.price} ${want.currency}`,
      );
    }

    // ---- 4. Agent 的文字回复也要落到对话流 ----
    await expect(page.locator('.chat-msg.chat-agent').last()).not.toBeEmpty();

    // ---- 5. 没有任何渲染错误 ----
    await expect(page.locator('.a2ui-gray-card')).toHaveCount(0);
    await expect(page.locator('.error')).toHaveCount(0);
    expect(consoleErrors, `控制台报错：\n${consoleErrors.join('\n')}`).toEqual([]);

    // ---- 6. 这一轮真的被记进了本地历史 ----
    //
    // 钉的是一个曾经整段失效、且上面所有断言都照不出来的缺口：recordRun 有实现、
    // 有导出、有单测，但**没有任何调用方**，于是订单页与历史页永远是空表。
    // 它坏在「没人调用」而不是「调用出错」，所以页面不报错、控制台不报错——
    // 只是永远显示「暂无订单」，看起来像新账号还没下过单。
    //
    // 断言用线上报文里的 run_id（后端回显的那个），不用前端自己生成的那个：
    // 两者若不是同一个，记下来的行就指向一个后端不认识的 id，详情页会 404。
    const runId = JSON.parse(frames[0]!.data)['run_id'] as string;
    expect(runId).toMatch(/^run_/);

    await page.getByRole('link', { name: '订单' }).click();
    await expect(page.getByRole('heading', { name: '订单' })).toBeVisible();
    await expect(
      page.locator('.orders-table'),
      `订单页没有 ${runId} 的行：recordRun 没被调用，或记在了 POST 成功之前`,
    ).toContainText(runId);

    // 同一份数据也该出现在历史页（两页读的是同一个 IndexedDB store）。
    await page.getByRole('link', { name: '历史' }).click();
    await expect(page.getByRole('heading', { name: '历史' })).toBeVisible();
    await expect(
      page.locator('.history-table'),
      `历史页没有 ${runId} 的行`,
    ).toContainText(runId);
  });
});

interface SseFrame {
  id: string;
  event: string;
  data: string;
}

// A2UI 报文的线上形态。只声明本文件用得到的字段，不做完整建模——
// 完整契约归后端 runevent.Validate* 与前端 schema.ts 管。
interface A2UIComponent {
  id: string;
  type: string;
  props?: Record<string, unknown>;
}

type A2UIMessage =
  | { action: 'createSurface'; components: A2UIComponent[] }
  | { action: 'updateComponents'; components: A2UIComponent[] }
  | { action: 'updateDataModel'; value: unknown };

type UpdateComponentsMessage = Extract<A2UIMessage, { action: 'updateComponents' }>;

// parseSseFrames 按空行切帧，逐行取 id / event / data。
function parseSseFrames(body: string): SseFrame[] {
  const frames: SseFrame[] = [];
  for (const raw of body.split('\n\n')) {
    const frame: SseFrame = { id: '', event: '', data: '' };
    for (const line of raw.split('\n')) {
      if (line.startsWith('id: ')) frame.id = line.slice(4);
      else if (line.startsWith('event: ')) frame.event = line.slice(7);
      else if (line.startsWith('data: ')) frame.data = line.slice(6);
    }
    if (frame.event !== '') frames.push(frame);
  }
  return frames;
}
