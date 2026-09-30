// 顶部导航 + 主内容容器。
//
// 所有 5 个路由共享这个 layout，导航 active 态由 NavLink 自动处理。
// 不在此处渲染业务内容——交给 children。

import { NavLink, Outlet } from 'react-router-dom';

const links: { to: string; label: string }[] = [
  { to: '/', label: '选购' },
  { to: '/orders', label: '订单' },
  { to: '/history', label: '历史' },
  { to: '/preferences', label: '偏好' },
];

export function AppLayout() {
  return (
    <div className="app-shell">
      <header className="app-header">
        <h1 className="app-title">CrossPilot</h1>
        <nav className="app-nav">
          {links.map((l) => (
            <NavLink
              key={l.to}
              to={l.to}
              end={l.to === '/'}
              className={({ isActive }) => `app-nav-link${isActive ? ' active' : ''}`}
            >
              {l.label}
            </NavLink>
          ))}
        </nav>
      </header>
      <main className="app-main">
        <Outlet />
      </main>
    </div>
  );
}
