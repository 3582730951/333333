// Parse every component file with the same parser Vite uses, and fail loudly on any that cannot
// be parsed.
//
// This exists because a file that does not compile is invisible to every other gate in `npm run
// check`. A `{/* comment */}` in a JSX *attribute list* is a syntax error -- `{` there begins a
// spread attribute, so the parser reports `Expected "..." but found "}"` -- and one sat in
// EmailPool.tsx through a full 31-page x 3-viewport x 2-theme overlap matrix that printed
// "clipped data: 0". It printed zero because Vite failed the transform, React never mounted, and
// the probe measured an empty document: the page's silence was indistinguishable from a pass.
//
// `tsc --noEmit` does catch it, but only for .ts/.tsx. This project sets checkJs: false and has
// ~74 .jsx files that tsc never reads, so the same mistake in any of those would reach a browser
// gate that structurally cannot detect it. This check covers both extensions, needs no network,
// and runs in about a second -- which is why it belongs first in the chain rather than after four
// minutes of browser work.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { transformWithOxc } from 'vite';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const srcDir = path.join(root, 'src');

function walk(dir) {
  const out = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...walk(full));
    else if (/\.(jsx|tsx|ts)$/.test(entry.name) && !entry.name.endsWith('.d.ts')) out.push(full);
  }
  return out;
}

const files = walk(srcDir).sort();
if (!files.length) {
  console.error('check-parse: found no source files under src/ -- the walk is broken, not the code');
  process.exit(1);
}

const failures = [];
await Promise.all(files.map(async (file) => {
  const code = fs.readFileSync(file, 'utf8');
  try {
    await transformWithOxc(code, file);
  } catch (error) {
    // oxc puts the caret diagram in the message; the first lines carry the location and reason.
    const detail = String(error?.message || error).split('\n').slice(0, 6).join('\n');
    failures.push({ file: path.relative(root, file), detail });
  }
}));

if (failures.length) {
  console.error(`check-parse: ${failures.length} file(s) failed to parse\n`);
  for (const { file, detail } of failures.sort((a, b) => a.file.localeCompare(b.file))) {
    console.error(`  ${file}`);
    for (const line of detail.split('\n')) console.error(`    ${line}`);
    console.error('');
  }
  process.exit(1);
}

console.log(`check-parse: ${files.length} source files parse cleanly`);
