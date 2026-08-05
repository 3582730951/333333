import fs from 'node:fs';
import path from 'node:path';
import { gzipSync } from 'node:zlib';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const dist = path.resolve(root, '../internal/console/dist');
const html = fs.readFileSync(path.join(dist, 'index.html'), 'utf8');
const initial = new Set();

for (const match of html.matchAll(/(?:src|href)="\/console\/(assets\/[^"?]+\.js)"/g)) {
  initial.add(match[1]);
}

const visit = (asset, graph = initial) => {
  const source = fs.readFileSync(path.join(dist, asset), 'utf8');
  const imports = [
    ...source.matchAll(/\b(?:from|export)\s*["'](\.\.?\/[^"']+\.js)["']/g),
    ...source.matchAll(/\bimport\s*["'](\.\.?\/[^"']+\.js)["']/g),
  ];
  for (const match of imports) {
    const dependency = path.posix.normalize(path.posix.join(path.posix.dirname(asset), match[1]));
    if (!graph.has(dependency)) {
      graph.add(dependency);
      visit(dependency, graph);
    }
  }
};

for (const asset of [...initial]) visit(asset);
const initialGzipBytes = [...initial].reduce((total, asset) => (
  total + gzipSync(fs.readFileSync(path.join(dist, asset))).length
), 0);
const assets = fs.readdirSync(path.join(dist, 'assets'));
const prefetchGraph = (entryPattern) => {
  const graph = new Set(initial);
  const entries = assets.filter((asset) => entryPattern.test(asset)).map((asset) => `assets/${asset}`);
  for (const asset of entries) {
    if (graph.has(asset)) continue;
    graph.add(asset);
    visit(asset, graph);
  }
  const gzipBytes = [...graph].reduce((total, asset) => total + gzipSync(fs.readFileSync(path.join(dist, asset))).length, 0);
  return { graph, gzipBytes };
};
const adminPrefetch = prefetchGraph(/^(?:Dashboard|Accounts|Usage|System|Keys|SettingsV2|Egress|Registration|TeamLifecycle)-.*\.js$/);
const portalPrefetch = prefetchGraph(/^(?:PortalDashboard|PortalKeys|PortalModels|PortalProfile)-.*\.js$/);
const chartChunk = [...initial].find((asset) => /(?:^|\/)(?:Charts|vendor-charts)-[^/]+\.js$/.test(asset));
const budget = 256 * 1024;

if (chartChunk) throw new Error(`lazy chart chunk entered initial dependency graph: ${chartChunk}`);
if (initialGzipBytes > budget) {
  throw new Error(`initial JavaScript gzip budget exceeded: ${initialGzipBytes} > ${budget}`);
}
console.log(`Build budget passed: ${initialGzipBytes} HTML initial static-graph JS gzip bytes across ${initial.size} assets`);
console.log(`Admin 3-second automatic prefetch cost: ${adminPrefetch.gzipBytes} cumulative JS gzip bytes across ${adminPrefetch.graph.size} assets (${adminPrefetch.gzipBytes - initialGzipBytes} bytes after initial)`);
console.log(`Portal 3-second automatic prefetch cost: ${portalPrefetch.gzipBytes} cumulative JS gzip bytes across ${portalPrefetch.graph.size} assets (${portalPrefetch.gzipBytes - initialGzipBytes} bytes after initial)`);
