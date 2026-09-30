// 路由表：与 App.tsx 的 <Route> 严格一一对应。
// P6 选 hash router（自研，无需 nginx try_files 配合）；
// P8 部署切 createBrowserRouter + nginx try_files。
export const ROUTES = [
  { path: '/',              name: 'shop'        as const },
  { path: '/orders',        name: 'orders'      as const },
  { path: '/orders/:runId', name: 'orderDetail' as const },
  { path: '/history',       name: 'history'     as const },
  { path: '/preferences',   name: 'preferences' as const },
] as const;

export type RouteName = (typeof ROUTES)[number]['name'];
