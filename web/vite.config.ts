import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, '.', 'ASTOVIA_DEV_');
  const apiURL = env.ASTOVIA_DEV_API_URL ?? 'http://localhost:8080';

  return {
    base: '/admin/',
    plugins: [react()],
    server: {
      watch: { usePolling: env.ASTOVIA_DEV_POLLING === 'true' },
      proxy: {
        '/auth': { target: apiURL, changeOrigin: false },
        '/api': { target: apiURL, changeOrigin: false },
        '/healthz': { target: apiURL, changeOrigin: false },
        '/mcp': { target: apiURL, changeOrigin: false },
        '/.well-known': { target: apiURL, changeOrigin: false },
        '/oauth': { target: apiURL, changeOrigin: false },
      },
    },
  };
});
