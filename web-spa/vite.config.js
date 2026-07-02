import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// The SPA is served by the Go backend under /console/ (alongside the legacy UI at /),
// so both base and the router basename are /console/. Dev proxies /admin and /v1 to the
// local pool server.
export default defineConfig({
  base: '/console/',
  plugins: [react()],
  build: {
    outDir: '../internal/console/dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (!id.includes('node_modules')) return undefined;
          if (id.includes('/react/') || id.includes('/react-dom/') || id.includes('/react-router-dom/') || id.includes('/scheduler/')) return 'vendor-react';
          if (id.includes('/recharts/') || id.includes('/d3-')) return 'vendor-charts';
          if (id.includes('/axios/')) return 'vendor-axios';
          return undefined;
        },
      },
    },
  },
  server: {
    port: 5188,
    proxy: {
      '/admin': 'http://127.0.0.1:8799',
      '/auth': 'http://127.0.0.1:8799',
      '/v1': 'http://127.0.0.1:8799',
      '/healthz': 'http://127.0.0.1:8799',
      '/file': 'http://127.0.0.1:8799',
    },
  },
});
