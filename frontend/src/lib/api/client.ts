// 极简 fetch 客户端（鉴权头按需注入）。
//
// 设计要点：
//   - 不引第三方 axios/ky：后端契约稳定、4 条路由足够，写死 fetch 就够。
//   - authToken 为空时不注入 Authorization（未启用鉴权的部署）。
//   - 错误统一抛 ApiError，UI 用 try/catch 渲染灰卡。

import { buildAuthHeaders } from '../auth';

export class ApiError extends Error {
  readonly status: number;
  readonly body: string;
  constructor(status: number, body: string) {
    super(`API ${status}: ${body.slice(0, 200)}`);
    this.name = 'ApiError';
    this.status = status;
    this.body = body;
  }
}

export interface ApiClientOptions {
  baseUrl: string;
  authToken?: string;
  sessionId?: string;
}

export class ApiClient {
  private readonly baseUrl: string;
  private readonly authToken: string;
  private readonly sessionId: string;

  constructor(opts: ApiClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/$/, '');
    this.authToken = opts.authToken ?? '';
    this.sessionId = opts.sessionId ?? '';
  }

  async get<T>(path: string, query?: Record<string, string | number | undefined>): Promise<T> {
    const url = this.resolveUrl(path, query);
    return this.request<T>('GET', url);
  }

  async post<T>(path: string, body?: unknown): Promise<T> {
    const url = this.resolveUrl(path);
    return this.request<T>('POST', url, body);
  }

  private resolveUrl(path: string, query?: Record<string, string | number | undefined>): URL {
    // baseUrl 为空时直接拿 path（dev proxy 模式下相对路径生效；测试时 jsdom 也走相对解析）。
    const url = this.baseUrl === '' ? new URL(path, window.location.href) : new URL(path, this.baseUrl);
    if (query) {
      for (const [k, v] of Object.entries(query)) {
        if (v !== undefined) url.searchParams.set(k, String(v));
      }
    }
    return url;
  }

  private async request<T>(method: string, url: URL, body?: unknown): Promise<T> {
    const headers: Record<string, string> = {
      Accept: 'application/json',
    };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    Object.assign(headers, buildAuthHeaders(this.authToken, this.sessionId));
    const init: RequestInit = { method, headers };
    if (body !== undefined) init.body = JSON.stringify(body);
    const res = await fetch(url.toString(), init);
    if (!res.ok) {
      const text = await safeRead(res);
      throw new ApiError(res.status, text);
    }
    return (await res.json()) as T;
  }
}

async function safeRead(res: Response): Promise<string> {
  try {
    return await res.text();
  } catch {
    return '';
  }
}

// 单例占位：vite proxy 模式下 baseUrl 用相对路径即可。
// 测试可显式 new ApiClient(...) 替换。
let _default: ApiClient | null = null;
export function defaultApi(): ApiClient {
  if (!_default) {
    _default = new ApiClient({ baseUrl: '' });
  }
  return _default;
}
