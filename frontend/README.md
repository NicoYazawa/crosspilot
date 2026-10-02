# CrossPilot Frontend

React 19 + Vite 8 + TypeScript 7.0 前端。

## 启动

```bash
npm install
npm run dev
# → http://localhost:5173
```

## 后端 CORS 配置

dev 期须设 `CROSSPILOT_HTTP_CORS_ORIGINS=http://localhost:5173`，
否则前端 fetch 跨域被拒。

## 自测

```bash
npm run lint      # ESLint（含禁词自检）
npm run test      # Vitest 单元/组件
npm run build     # TS 严格通过 + 静态产物
npm run e2e       # Playwright（mock 后端事件，无需真实模型）
```

## 目录骨架

```
src/
├── app/          # 顶层 shell / 路由 / ErrorBoundary
├── features/     # 业务视图（shop / a2ui / observability）
├── lib/          # SSE 客户端 / 鉴权头（Bearer JWT）/ cursor
├── components/   # 通用组件
├── types/        # 后端 JSON 对应 TS 类型（与 snake_case 一一对应）
└── test/         # vitest setup
```
