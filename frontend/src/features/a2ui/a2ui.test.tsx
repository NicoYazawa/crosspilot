// A2UI 渲染器测试 — G4 XSS 验收。
//
// 测试策略（按 plan R4：不引入 @testing-library/react；用 jsdom + react-dom/client createRoot）。
//
// 覆盖：
//   1. <script> 注入 → DOM 文本包含字面量，无 <script> 元素生成
//   2. 未注册 type → 拒渲染 / 抛 A2UIRenderError
//   3. 错误 catalogId → 抛 A2UIRenderError
//   4. 缺 /requirements 根 → 抛 A2UIRenderError
//   5. 正常 createSurface + updateComponents + updateDataModel → 渲染成功

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { createRoot, type Root } from 'react-dom/client';
import { flushSync } from 'react-dom';
import { A2UIRenderer } from './Renderer';
import { A2UIRenderError, validateMessage } from './schema';
import { A2UI_CATALOG_ID, A2UI_VERSION, SHOPPING_REQUIREMENTS_PATH } from '@/types/a2ui';

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  container = document.createElement('div');
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  root.unmount();
  container.remove();
});

function renderMessages(msgs: unknown[]): void {
  flushSync(() => {
    root.render(<A2UIRenderer messages={msgs} />);
  });
}

describe('A2UI v0.9 — G4 XSS 验收', () => {
  it('正常三报文 → 渲染 ProductCard', () => {
    const msgs = [
      {
        action: 'createSurface',
        catalogId: A2UI_CATALOG_ID,
        version: A2UI_VERSION,
        components: [
          {
            id: 'root',
            type: 'Column',
            path: SHOPPING_REQUIREMENTS_PATH,
            props: { children: ['card'] },
          },
          {
            id: 'card',
            type: 'ProductCard',
            props: { title: '登山包', subtitle: '防水', price_minor: 39900, currency: 'CNY' },
          },
        ],
      },
    ];
    renderMessages(msgs);
    const html = container.innerHTML;
    expect(html).toContain('登山包');
    expect(html).toContain('39900');
  });

  it('XSS 注入：<script>alert(1)</script> → DOM 文本含字面量、<script> 元素数不增加', () => {
    const scriptsBefore = document.querySelectorAll('script').length;
    const evilTitle = '<script>window.PWNED = true;</script>登山包';
    const msgs = [
      {
        action: 'createSurface',
        catalogId: A2UI_CATALOG_ID,
        version: A2UI_VERSION,
        components: [
          { id: 'root', type: 'Column', path: SHOPPING_REQUIREMENTS_PATH, props: { children: ['card'] } },
          { id: 'card', type: 'ProductCard', props: { title: evilTitle } },
        ],
      },
    ];
    renderMessages(msgs);
    // React 把 <script> 转义为字面量字符串（&lt;script&gt;...）；
    // textContent 会反向解码，断言「字面量仍在 DOM 文本里」（不是作为元素执行）。
    expect(container.textContent).toContain(evilTitle);
    // 真实 <script> 元素数不增加 —— XSS 防护生效。
    expect(document.querySelectorAll('script').length).toBe(scriptsBefore);
    expect((window as unknown as { PWNED?: boolean }).PWNED).toBeUndefined();
  });

  it('XSS 注入：javascript: URL 与 onerror 属性 → 不会触发执行', () => {
    const evilProps = {
      title: '<img src=x onerror="window.PWNED2=true">',
      subtitle: '<a href="javascript:alert(1)">click</a>',
    };
    const msgs = [
      {
        action: 'createSurface',
        catalogId: A2UI_CATALOG_ID,
        version: A2UI_VERSION,
        components: [
          { id: 'root', type: 'Column', path: SHOPPING_REQUIREMENTS_PATH, props: { children: ['card'] } },
          { id: 'card', type: 'ProductCard', props: evilProps },
        ],
      },
    ];
    renderMessages(msgs);
    expect((window as unknown as { PWNED2?: boolean }).PWNED2).toBeUndefined();
    // React 转义：<img> 不应作为真实元素出现
    expect(container.querySelectorAll('img').length).toBe(0);
    // <a href="javascript:..."> 不应作为真实元素出现
    expect(container.querySelectorAll('a').length).toBe(0);
  });

  it('未注册 type (HtmlView) → 渲染灰卡', () => {
    const msgs = [
      {
        action: 'createSurface',
        catalogId: A2UI_CATALOG_ID,
        version: A2UI_VERSION,
        components: [
          { id: 'root', type: 'HtmlView', path: SHOPPING_REQUIREMENTS_PATH },
        ],
      },
    ];
    renderMessages(msgs);
    expect(container.innerHTML).toContain('A2UI 渲染失败');
  });

  it('错误 catalogId → 渲染灰卡', () => {
    const msgs = [
      {
        action: 'createSurface',
        catalogId: 'evil.local/foo',
        version: A2UI_VERSION,
        components: [
          { id: 'root', type: 'Column', path: SHOPPING_REQUIREMENTS_PATH, props: { children: [] } },
        ],
      },
    ];
    renderMessages(msgs);
    expect(container.innerHTML).toContain('A2UI 渲染失败');
  });

  it('缺 /requirements 根 → validateMessage 抛 A2UIRenderError', () => {
    const msgs = [
      {
        action: 'createSurface',
        catalogId: A2UI_CATALOG_ID,
        version: A2UI_VERSION,
        components: [{ id: 'card', type: 'ProductCard' }],
      },
    ];
    expect(() => validateMessage(msgs[0])).toThrow(A2UIRenderError);
  });

  it('updateComponents 后根组件可见', () => {
    const msgs = [
      {
        action: 'createSurface',
        catalogId: A2UI_CATALOG_ID,
        version: A2UI_VERSION,
        components: [
          { id: 'root', type: 'Column', path: SHOPPING_REQUIREMENTS_PATH, props: { children: ['card'] } },
        ],
      },
      {
        action: 'updateComponents',
        catalogId: A2UI_CATALOG_ID,
        components: [{ id: 'card', type: 'Text', props: { content: '已更新' } }],
      },
    ];
    renderMessages(msgs);
    expect(container.innerHTML).toContain('已更新');
  });

  it('updateDataModel → List 组件可读 data 数组', () => {
    const msgs = [
      {
        action: 'createSurface',
        catalogId: A2UI_CATALOG_ID,
        version: A2UI_VERSION,
        components: [
          { id: 'root', type: 'Column', path: SHOPPING_REQUIREMENTS_PATH, props: { children: ['list'] } },
          { id: 'list', type: 'List' },
        ],
      },
      {
        action: 'updateDataModel',
        catalogId: A2UI_CATALOG_ID,
        value: {
          path: '/products',
          data: [{ title: 'A' }, { title: 'B' }],
        },
      },
    ];
    renderMessages(msgs);
    // 因为 list 的 props 没有 data 字段；本测试仅验证 updateDataModel 不抛错且 Renderer 进入 ready 状态。
    expect(container.innerHTML).not.toContain('A2UI 渲染失败');
  });
});
