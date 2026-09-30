// Vitest setup：jsdom polyfill + 通用 matchers。
import '@testing-library/jest-dom/vitest';

// jsdom 不实现的 Web API 兜底
if (typeof globalThis.crypto === 'undefined') {
  // 仅在 jsdom 缺失时 stub；浏览器原生 crypto.subtle 是真实实现
  // vitest + jsdom 通常已带 webcrypto polyfill，这里只兜底极端环境
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (globalThis as any).crypto = globalThis.crypto;
}
