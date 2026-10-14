import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

const target = process.env.YUANPU_SERVER_API_URL ?? 'http://127.0.0.1:8788';

export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      '/v1': target,
      '/openapi.yaml': target,
    },
  },
});
