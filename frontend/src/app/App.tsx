import { HashRouter, Link, Route, Routes } from 'react-router-dom';
import { ErrorBoundary } from './ErrorBoundary';
import { ShopView } from '@/features/shop/ShopView';
import { OrdersView } from '@/features/orders/OrdersView';
import { OrderDetailView } from '@/features/orders/OrderDetailView';
import { HistoryView } from '@/features/history/HistoryView';
import { PreferencesView } from '@/features/preferences/PreferencesView';

export function App() {
  return (
    <ErrorBoundary>
      <HashRouter>
        <nav style={{ padding: '12px 24px', borderBottom: '1px solid #ddd' }}>
          <Link to="/" style={{ marginRight: 16 }}>选购</Link>
          <Link to="/orders" style={{ marginRight: 16 }}>订单</Link>
          <Link to="/history" style={{ marginRight: 16 }}>历史</Link>
          <Link to="/preferences">偏好</Link>
        </nav>
        <main style={{ padding: 24 }}>
          <Routes>
            <Route path="/" element={<ShopView />} />
            <Route path="/orders" element={<OrdersView />} />
            <Route path="/orders/:runId" element={<OrderDetailView />} />
            <Route path="/history" element={<HistoryView />} />
            <Route path="/preferences" element={<PreferencesView />} />
          </Routes>
        </main>
      </HashRouter>
    </ErrorBoundary>
  );
}
