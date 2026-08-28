import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { readdirSync, readFileSync, statSync, writeFileSync } from 'node:fs';
import { extname, join } from 'node:path';
import { brotliCompressSync, constants as zlibConstants } from 'node:zlib';

import { computeSourceDigest } from './scripts/source-digest.mjs';

function sourceFreshnessManifest() {
  return {
    name: 'pool-source-freshness-manifest',
    apply: 'build',
    closeBundle() {
      const output = join(process.cwd(), '../internal/console/dist/build-meta.json');
      writeFileSync(output, `${JSON.stringify({
        schema: 1,
        source_sha256: computeSourceDigest(),
      }, null, 2)}\n`);
    },
  };
}

function brotliPrecompress() {
  return {
    name: 'pool-brotli-precompress',
    apply: 'build',
    closeBundle() {
      const root = join(process.cwd(), '../internal/console/dist');
      const visit = (directory) => {
        for (const name of readdirSync(directory)) {
          const file = join(directory, name);
          if (statSync(file).isDirectory()) {
            visit(file);
            continue;
          }
          if (!['.js', '.css', '.json', '.svg', '.html'].includes(extname(file)) || file.endsWith('.br')) continue;
          const source = readFileSync(file);
          if (source.length < 1024) continue;
          writeFileSync(`${file}.br`, brotliCompressSync(source, {
            params: {
              [zlibConstants.BROTLI_PARAM_QUALITY]: 11,
              [zlibConstants.BROTLI_PARAM_MODE]: zlibConstants.BROTLI_MODE_TEXT,
            },
          }));
        }
      };
      visit(root);
    },
  };
}

// The SPA is served by the Go backend under /console/ (alongside the legacy UI at /),
// so both base and the router basename are /console/. Dev proxies /admin and /v1 to the
// local pool server.
export default defineConfig({
  base: '/console/',
  plugins: [react(), sourceFreshnessManifest(), brotliPrecompress()],
  build: {
    target: 'es2022',
    outDir: '../internal/console/dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
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
