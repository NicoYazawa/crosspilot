import { HashRouter, Route, Routes } from 'react-router-dom';
import { ErrorBoundary } from './ErrorBoundary';
import { AppLayout } from './AppLayout';
import { ShopView } from '@/features/shop/ShopView';
import { OrdersView } from '@/features/orders/OrdersView';
import { OrderDetailView } from '@/features/orders/OrderDetailView';
import { HistoryView } from '@/features/history/HistoryView';
import { PreferencesView } from '@/features/preferences/PreferencesView';

export function App() {
  return (
    <ErrorBoundary>
      <HashRouter>
        <Routes>
          <Route element={<AppLayout />}>
            <Route path="/" element={<ShopView />} />
            <Route path="/orders" element={<OrdersView />} />
            <Route path="/orders/:runId" element={<OrderDetailView />} />
            <Route path="/history" element={<HistoryView />} />
            <Route path="/preferences" element={<PreferencesView />} />
          </Route>
        </Routes>
      </HashRouter>
    </ErrorBoundary>
  );
}
