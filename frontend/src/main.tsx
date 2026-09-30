import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './app/App';
import './index.css';

const rootEl = document.getElementById('root');
if (!rootEl) {
  throw new Error('root 元素不存在（index.html 应有 <div id="root"></div>）');
}

createRoot(rootEl).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
