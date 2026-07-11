import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const app = fs.readFileSync(path.join(root, 'src', 'App.tsx'), 'utf8');
const routes = fs.readFileSync(path.join(root, 'src', 'app', 'routeDefinitions.ts'), 'utf8');
const failures = [];

if (!routes.includes("prefetch: 'eager'") || !routes.includes("prefetch: 'idle'")) {
  failures.push('route metadata must declare eager and idle prefetch policies.');
}
if (!app.includes('reportClientError') || !app.includes('isChunkLoadError') || !app.includes('notifyChunkUpdateAvailable')) {
  failures.push('route prefetch failures must report client errors and identify chunk updates.');
}
if (!/lazyLoader\(\)\.catch\(\(error\) => reportPrefetchError\(error, route\)\)/.test(app)) {
  failures.push('route lazy-loader catch handlers must call reportPrefetchError.');
}

if (failures.length) {
  console.error('Route prefetch error handling check failed:');
  failures.forEach((failure) => console.error(`- ${failure}`));
  process.exit(1);
}
console.log('Route prefetch error handling check passed.');
