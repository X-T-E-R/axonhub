import path from 'node:path';
import { readFileSync } from 'node:fs';
import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react-swc';
import tailwindcss from '@tailwindcss/vite';

const fixture = JSON.parse(readFileSync(process.env.AXON_RAW_BROWSER_FIXTURE!, 'utf8'));
if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(fixture.baseURL)) throw new Error('Use only the synthetic loopback fixture');

export default defineConfig({
  root: path.resolve(import.meta.dirname, '../..'),
  cacheDir: path.resolve(import.meta.dirname, '../../../.runtime/raw-vite-cache'),
  plugins: [
    react(),
    tailwindcss(),
    {
      name: 'synthetic-raw-fixture',
      configureServer(server) {
        server.middlewares.use('/__raw_fixture', (_req, res) => {
          res.setHeader('Content-Type', 'application/json');
          res.end(JSON.stringify(fixture));
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
  server: { host: '127.0.0.1', port: 5194, strictPort: true, proxy: { '/admin': fixture.baseURL } },
});
