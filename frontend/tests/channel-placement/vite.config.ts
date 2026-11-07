import path from 'node:path';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react-swc';
import tailwindcss from '@tailwindcss/vite';
import {
  clearRequestLog,
  getRequestLog,
  handleGraphQLRequest,
  handlePortalGraphQLRequest,
} from './mock-server';

const root = path.resolve(import.meta.dirname, '../..');

export default defineConfig({
  root,
  cacheDir: path.resolve(import.meta.dirname, '../../../.runtime/channel-placement-vite-cache'),
  plugins: [
    react(),
    tailwindcss(),
    {
      name: 'synthetic-channel-placement-fixture',
      configureServer(server) {
        server.middlewares.use(async (request, response, next) => {
          const pathname = new URL(request.url ?? '/', 'http://127.0.0.1').pathname;

          if (pathname === '/admin/graphql') {
            await handleGraphQLRequest(request, response);
            return;
          }

          if (pathname === '/portal/graphql') {
            await handlePortalGraphQLRequest(request, response);
            return;
          }

          if (pathname === '/__channel_placement/requests' && request.method === 'GET') {
            response.statusCode = 200;
            response.setHeader('Content-Type', 'application/json; charset=utf-8');
            response.end(JSON.stringify({ requests: getRequestLog() }));
            return;
          }

          if (pathname === '/__channel_placement/requests' && request.method === 'DELETE') {
            clearRequestLog();
            response.statusCode = 204;
            response.end();
            return;
          }

          next();
        });
      },
    },
  ],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, '../../src'),
      '@tabler/icons-react': '@tabler/icons-react/dist/esm/icons/index.mjs',
    },
  },
  server: {
    host: '127.0.0.1',
    port: 5195,
    strictPort: true,
  },
});
