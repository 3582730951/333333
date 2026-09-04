// Budget gate for the progressive rendering engine.
//
// Artifact convention (kept here next to the check so it is auditable): an engine chunk is any
// dist/assets/**/*.js whose filename contains `engine` (case-insensitive), or an artifact mapped
// from the source module `src/engine/index.js` by Vite's manifest/source map. A future bundler may
// also carry the explicit `@aurora-engine` marker in that chunk. Dynamic import() is intentionally
// not followed when building the first-screen graph; the engine must stay out of that graph.
//
// The first-screen shell convention is likewise explicit: an HTML `data-aurora-shell` marker or
// a chunk filename containing shell/capability/fallback/bootstrap, provided that chunk is in the
// HTML static graph. The shell is the only capability probe + fallback surface and is checked at
// 8 KiB gzip. The regular build budget gate remains responsible for the existing 256 KiB initial
// static graph; this file owns only the engine and shell budgets.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const defaultDist = path.resolve(repoRoot, '../internal/console/dist');
const engineBudget = 150 * 1024;
const engineTotalBudget = 153600;
const shellBudget = 8 * 1024;

function optionValue(name) {
  const index = process.argv.indexOf(name);
  if (index >= 0) return process.argv[index + 1] || '';
  const prefix = `${name}=`;
  const inline = process.argv.find((argument) => argument.startsWith(prefix));
  return inline ? inline.slice(prefix.length) : '';
}

const dist = path.resolve(optionValue('--dist') || process.env.ENGINE_DIST_DIR || process.env.ENGINE_DIST || defaultDist);
const assetsDir = path.join(dist, 'assets');

function walkJavaScript(directory) {
  if (!fs.existsSync(directory)) return [];
  const files = [];
  for (const entry of fs.readdirSync(directory, { withFileTypes: true })) {
    const fullPath = path.join(directory, entry.name);
    if (entry.isDirectory()) files.push(...walkJavaScript(fullPath));
    else if (entry.isFile() && entry.name.endsWith('.js')) files.push(fullPath);
  }
  return files.sort((left, right) => left.localeCompare(right));
}

function relativeAsset(file) {
  return path.relative(dist, file).split(path.sep).join('/');
}

const chunkFiles = walkJavaScript(assetsDir);
const chunks = new Map(chunkFiles.map((file) => [relativeAsset(file), file]));
console.log(`Engine budget scan: 扫描到 ${chunks.size} 个 chunk (${path.relative(repoRoot, dist) || '.'}/assets)`);

function normalizeAssetReference(reference) {
  if (typeof reference !== 'string') return '';
  let normalized = reference.split(/[?#]/, 1)[0].replaceAll('\\', '/');
  const assetsIndex = normalized.indexOf('assets/');
  if (assetsIndex >= 0) normalized = normalized.slice(assetsIndex);
  return normalized.replace(/^\.\//, '').replace(/^\//, '');
}

function addMappedChunk(reference, reason, candidates, discoveryProblems) {
  const normalized = normalizeAssetReference(reference);
  if (!normalized || !normalized.endsWith('.js')) return;
  if (chunks.has(normalized)) {
    if (!candidates.has(normalized)) candidates.set(normalized, reason);
    return;
  }
  const sameBasename = [...chunks.keys()].filter((name) => path.posix.basename(name) === path.posix.basename(normalized));
  if (sameBasename.length === 1) {
    if (!candidates.has(sameBasename[0])) candidates.set(sameBasename[0], reason);
  } else {
    discoveryProblems.push(`引擎映射指向不存在的 chunk: ${reference}`);
  }
}

function readJson(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    return undefined;
  }
}

function manifestEngineFiles(value, key, references) {
  if (Array.isArray(value)) {
    for (const item of value) manifestEngineFiles(item, '', references);
    return;
  }
  if (!value || typeof value !== 'object') {
    if (typeof value === 'string' && /(?:^|\/)src\/engine\/index\.js$/.test(key)) references.push(value);
    return;
  }
  const sourceKeys = [key, typeof value.src === 'string' ? value.src : ''];
  if (sourceKeys.some((sourceKey) => /(?:^|\/)src\/engine\/index\.js(?:$|\?)/.test(sourceKey))) {
    if (typeof value.file === 'string') references.push(value.file);
    if (typeof value.href === 'string') references.push(value.href);
  }
  for (const [childKey, childValue] of Object.entries(value)) {
    manifestEngineFiles(childValue, childKey, references);
  }
}

function discoverEngineChunks() {
  const candidates = new Map();
  const discoveryProblems = [];
  for (const [name, file] of chunks) {
    if (/engine/i.test(path.posix.basename(name))) candidates.set(name, 'filename contains engine');
    try {
      const source = fs.readFileSync(file, 'utf8');
      if (/@aurora-engine\b|src\/engine\/index\.js/.test(source)) {
        if (!candidates.has(name)) candidates.set(name, 'explicit engine marker/source reference');
      }
    } catch (error) {
      discoveryProblems.push(`无法读取 chunk ${name}: ${error.message}`);
    }
    const mapFile = `${file}.map`;
    if (fs.existsSync(mapFile)) {
      const map = readJson(mapFile);
      if (map && Array.isArray(map.sources)
        && map.sources.some((source) => /(?:^|[\\/])src[\\/]engine[\\/]index\.js$/.test(source))) {
        candidates.set(name, 'source map maps src/engine/index.js');
      }
    }
  }

  const manifestFiles = [
    path.join(dist, 'manifest.json'),
    path.join(dist, '.vite', 'manifest.json'),
    path.join(assetsDir, 'manifest.json'),
  ].filter((file) => fs.existsSync(file));
  for (const manifestFile of manifestFiles) {
    const manifest = readJson(manifestFile);
    if (!manifest) {
      discoveryProblems.push(`无法解析 manifest: ${path.relative(dist, manifestFile)}`);
      continue;
    }
    const references = [];
    manifestEngineFiles(manifest, '', references);
    for (const reference of references) {
      addMappedChunk(reference, `manifest maps src/engine/index.js (${path.relative(dist, manifestFile)})`, candidates, discoveryProblems);
    }
  }
  return { candidates, discoveryProblems };
}

function initialReferences(html) {
  const references = new Set();
  for (const match of html.matchAll(/<(?:script\b[^>]*?\bsrc|link\b[^>]*?\bhref)\s*=\s*["']([^"']+\.js(?:[?#][^"']*)?)["']/gi)) {
    const normalized = normalizeAssetReference(match[1]);
    if (normalized) references.add(normalized);
  }
  return references;
}

function staticGraph(initial) {
  const graph = new Set(initial);
  const visit = (asset) => {
    if (!chunks.has(asset)) return;
    const source = fs.readFileSync(chunks.get(asset), 'utf8');
    const imports = [
      ...source.matchAll(/\b(?:from|export)\s*["'](\.\.?\/[^"']+\.js)["']/g),
      ...source.matchAll(/\bimport\s*["'](\.\.?\/[^"']+\.js)["']/g),
    ];
    for (const match of imports) {
      const dependency = path.posix.normalize(path.posix.join(path.posix.dirname(asset), match[1]));
      if (!graph.has(dependency)) {
        graph.add(dependency);
        visit(dependency);
      }
    }
  };
  for (const asset of [...initial]) visit(asset);
  return graph;
}

function shellReferences(html, graph) {
  const marked = [];
  for (const match of html.matchAll(/data-aurora-shell\s*=\s*["']([^"']+)["']/gi)) marked.push(match[1]);
  for (const match of html.matchAll(/aurora-shell\s*:\s*([^\s*"']+\.js)/gi)) marked.push(match[1]);
  const explicit = [...new Set(marked.map(normalizeAssetReference).filter(Boolean))];
  if (explicit.length) return explicit;
  return [...graph].filter((asset) => /(?:^|[-_.])(?:shell|capability|fallback|bootstrap)(?:[-_.]|$)/i.test(path.posix.basename(asset)));
}

const { candidates: engineChunks, discoveryProblems } = discoverEngineChunks();
const failures = [...discoveryProblems];

// Discovering nothing used to exit 0. That made the gate vacuous for the whole
// of P3-P5: this repository has no `src/engine/index.js`, emits no source map
// and no Vite manifest, so not one discovery rule could ever match and the gate
// reported "passed" while weighing zero bytes. Now that P6 has wired the engine
// in, an empty result is a broken gate, not an absent engine.
if (!engineChunks.size) {
  failures.push(`未发现任何引擎 chunk（已扫描 ${chunks.size} 个）；引擎已在 P6 接线，`
    + '此处为空说明发现规则失效，而不是引擎不存在');
}

let engineTotalBytes = 0;
for (const [name, reason] of engineChunks) {
  const bytes = gzipSync(fs.readFileSync(chunks.get(name))).length;
  engineTotalBytes += bytes;
  console.log(`  Engine ${name}: ${bytes} gzip bytes (${reason})`);
  if (bytes > engineBudget) failures.push(`引擎 chunk ${name} gzip ${bytes} > ${engineBudget}`);
}
// P3 §3 budgets the engine as a whole, not chunk by chunk: 40 effects that each
// slip under a per-chunk ceiling still add up.
console.log(`Engine total: ${engineTotalBytes} gzip bytes across ${engineChunks.size} chunk(s) (budget ${engineTotalBudget})`);
if (engineTotalBytes > engineTotalBudget) {
  failures.push(`引擎总量 gzip ${engineTotalBytes} > ${engineTotalBudget}（P3 §3 动态引擎合计）`);
}

const htmlFile = path.join(dist, 'index.html');
if (!fs.existsSync(htmlFile)) {
  failures.push('未找到 index.html，无法确认首屏壳是否进入 initial static-graph');
} else {
  const html = fs.readFileSync(htmlFile, 'utf8');
  const initial = initialReferences(html);
  const graph = staticGraph(initial);
  // The filename heuristic also matches the console's own `bootstrap-*.js`,
  // which is not an Aurora chunk at all. An Aurora shell is by definition one of
  // the discovered engine chunks, so intersecting removes the false positive
  // without weakening the explicit `data-aurora-shell` path.
  const shellCandidates = [...new Set(shellReferences(html, graph))];
  const shells = shellCandidates.filter((name) => engineChunks.has(name));
  const ignoredShells = shellCandidates.filter((name) => !engineChunks.has(name));
  if (ignoredShells.length) {
    console.log(`  (ignored non-Aurora shell-shaped chunk(s): ${ignoredShells.join(', ')})`);
  }
  if (!shells.length) {
    failures.push('未找到首屏壳；请使用 data-aurora-shell 或 shell/capability/fallback/bootstrap 文件名约定');
  } else {
    for (const shell of shells) {
      if (!chunks.has(shell)) {
        failures.push(`首屏壳不在构建产物中: ${shell}`);
        continue;
      }
      if (!graph.has(shell)) {
        failures.push(`首屏壳未进入 initial static-graph: ${shell}`);
        continue;
      }
      const bytes = gzipSync(fs.readFileSync(chunks.get(shell))).length;
      console.log(`  First-screen shell ${shell}: ${bytes} gzip bytes (budget ${shellBudget})`);
      if (bytes > shellBudget) failures.push(`首屏壳 ${shell} gzip ${bytes} > ${shellBudget}`);
    }
  }

  // P3 §3: the shell is the ONLY Aurora entry allowed in the first-screen graph.
  // Everything else -- host, gl, compositor, all 40 effects -- must arrive by
  // dynamic import, and this is the assertion that keeps a stray static import
  // from quietly moving the whole renderer onto the critical path.
  const shellSet = new Set(shells);
  const leaked = [...engineChunks.keys()].filter((name) => graph.has(name) && !shellSet.has(name));
  if (leaked.length) {
    failures.push(`以下引擎 chunk 进入了 initial static-graph（只允许首屏壳）: ${leaked.join(', ')}`);
  }
  console.log(`Engine static-graph leak check: ${leaked.length} leaked / ${engineChunks.size} engine chunk(s), `
    + `shell(s) ${shells.join(', ') || '(none)'}`);
}

if (failures.length) {
  console.error(`Engine budget check failed: ${failures.length} violation(s)`);
  for (const failure of failures) console.error(`  ${failure}`);
  process.exit(1);
}

console.log(`Engine budget passed: ${engineChunks.size} engine chunk(s), total ${engineTotalBytes} <= ${engineTotalBudget} gzip bytes; `
  + `each <= ${engineBudget}; first-screen shell <= ${shellBudget} gzip bytes`);
