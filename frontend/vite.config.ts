import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import path from 'node:path';

// Vite 配置：dev proxy → 后端 :8000；alias @ → src
export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: { '@': path.resolve(import.meta.dirname, 'src') },
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      // /commerce 同时覆盖交易链路与 AG-UI 子应用（/commerce/ag-ui/*）。
      '/commerce':      { target: 'http://localhost:8000', changeOrigin: true },
      '/observability': { target: 'http://localhost:8000', changeOrigin: true },
    },
  },
  build: {
    target: 'es2022',
    sourcemap: true,
    rollupOptions: {
      output: {
        manualChunks(id: string) {
          if (id.includes('node_modules/recharts')) return 'recharts';
          if (id.includes('node_modules/react-router')) return 'router';
          return undefined;
        },
      },
    },
  },
});
