// Hardcoded-CJK ratchet for P0 Top10 #3.
//
// 2,120 user-facing Chinese strings sit outside the locale tables. That is far
// too many to migrate in one pass, and a botched migration silently rewrites
// what users read -- so this gate does not demand zero. It demands that the
// number never grows, so the backlog can be paid down file by file without the
// next feature quietly adding to it.
//
// Counting is done with Babel, not a regex. A regex over source text cannot tell
// a user-facing JSXText from a comment, an import path, or a locale key, and a
// previous measurement in this repository reached a wrong conclusion that
// outlived the tool that produced it.
//
// A file this gate cannot parse is a failure, not a skip: silently continuing is
// how a coverage gate reports a smaller, fully-passing denominator.
import { readFileSync, readdirSync, statSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { parse } from '@babel/parser';
import _traverse from '@babel/traverse';

const traverse = _traverse.default || _traverse;
const root = path.resolve(import.meta.dirname, '..');
const sourceRoot = path.join(root, 'src');
const baselineFile = path.join(root, 'scripts', 'i18n-coverage-baseline.json');
const CJK_PATTERN = /[一-鿿㐀-䶿]/;
const CJK_GLOBAL = /[一-鿿㐀-䶿]/g;
const update = process.argv.includes('--update');

function sourceFiles(directory, found = []) {
  for (const name of readdirSync(directory)) {
    const full = path.join(directory, name);
    if (statSync(full).isDirectory()) {
      // The locale tables are the destination, not a violation.
      if (name !== 'locales') sourceFiles(full, found);
      continue;
    }
    if (/\.(jsx?|tsx?)$/.test(name) && !name.endsWith('.d.ts')) found.push(full);
  }
  return found;
}

const files = sourceFiles(sourceRoot).sort();
const perFile = {};
const parseFailures = [];
let totalSites = 0;
let totalChars = 0;

for (const file of files) {
  const relative = path.relative(root, file).split(path.sep).join('/');
  const source = readFileSync(file, 'utf8');
  if (!CJK_PATTERN.test(source)) continue;
  let ast;
  try {
    ast = parse(source, {
      sourceType: 'module',
      plugins: ['jsx', 'typescript', 'decorators-legacy', 'classProperties'],
    });
  } catch (error) {
    parseFailures.push(`${relative}: ${error.message}`);
    continue;
  }
  let sites = 0;
  let chars = 0;
  const record = (value) => {
    if (typeof value !== 'string' || !CJK_PATTERN.test(value)) return;
    sites += 1;
    chars += (value.match(CJK_GLOBAL) || []).length;
  };
  traverse(ast, {
    StringLiteral(nodePath) {
      if (nodePath.parentPath.isImportDeclaration()) return;
      record(nodePath.node.value);
    },
    TemplateElement(nodePath) { record(nodePath.node.value.cooked ?? nodePath.node.value.raw); },
    JSXText(nodePath) { record(nodePath.node.value.trim()); },
  });
  if (sites) {
    perFile[relative] = sites;
    totalSites += sites;
    totalChars += chars;
  }
}

// The denominator is printed whatever happens, so a shrinking scan can never be
// mistaken for a passing one.
console.log(`i18n coverage: scanned ${files.length} source file(s); `
  + `${Object.keys(perFile).length} still carry hardcoded CJK (${totalSites} sites / ${totalChars} chars)`);

if (parseFailures.length) {
  console.error(`i18n coverage check failed: ${parseFailures.length} file(s) could not be parsed`);
  for (const failure of parseFailures) console.error(`  ${failure}`);
  process.exit(1);
}

if (update) {
  writeFileSync(baselineFile, `${JSON.stringify({
    note: 'Ratchet for P0 Top10 #3. Regenerate with: npm run check:i18n-coverage -- --update',
    total_sites: totalSites,
    total_chars: totalChars,
    files: perFile,
  }, null, 2)}\n`);
  console.log(`i18n coverage baseline written: ${path.relative(root, baselineFile)}`);
  process.exit(0);
}

let baseline;
try {
  baseline = JSON.parse(readFileSync(baselineFile, 'utf8'));
} catch (error) {
  console.error(`i18n coverage check failed: baseline unreadable (${error.message}); `
    + 'run `npm run check:i18n-coverage -- --update` to create it');
  process.exit(1);
}

const failures = [];
if (totalSites > baseline.total_sites) {
  failures.push(`总量增加: ${baseline.total_sites} → ${totalSites} 处硬编码中文`);
}
for (const [file, count] of Object.entries(perFile)) {
  const before = baseline.files[file] ?? 0;
  if (count > before) failures.push(`${file}: ${before} → ${count}`);
}

const improved = Object.entries(baseline.files)
  .filter(([file, count]) => (perFile[file] ?? 0) < count).length;
if (improved) console.log(`  ${improved} file(s) improved since the baseline`);

if (failures.length) {
  console.error(`i18n coverage check failed: ${failures.length} regression(s)`);
  for (const failure of failures) console.error(`  ${failure}`);
  console.error('  抽到 src/lib/locales 后重跑；确有必要放宽时用 --update 重建基线。');
  process.exit(1);
}

console.log(`i18n coverage check passed: ${totalSites}/${baseline.total_sites} sites (ratchet holds)`);
