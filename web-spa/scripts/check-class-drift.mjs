#!/usr/bin/env node
// Three bugs this pass came from the same crack: a class or custom property that one layer
// writes and no other layer reads. `pool-text-clamp--multi` had no CSS, so `lines={2}` was
// silently a no-op; `pool-table-layout-fit` had none either, and twelve pages passed it
// expecting behaviour. Neither shows up in a typecheck, a unit test, or a screenshot that
// happens not to exercise the case. This finds them by comparing what the components emit
// against what the stylesheets define.
import { readFileSync, writeFileSync } from 'node:fs';
import { execSync } from 'node:child_process';
import path from 'node:path';

const root = path.resolve(import.meta.dirname, '..');
const list = (pattern, dir) => execSync(`grep -rl "" --include=${pattern} ${dir}`, { cwd: root, encoding: 'utf8' })
  .trim().split('\n').filter(Boolean);

const styleFiles = list('*.css', 'src/styles');
const codeFiles = [...list('*.jsx', 'src'), ...list('*.tsx', 'src')];

const css = styleFiles.map((f) => readFileSync(path.join(root, f), 'utf8')).join('\n');
// Class names as they appear in selectors, and custom properties as they are declared.
const definedClasses = new Set([...css.matchAll(/\.(pool-[a-z0-9_-]+)/g)].map((m) => m[1]));
const definedVars = new Set([...css.matchAll(/(--pool-[a-z0-9-]+)\s*:/g)].map((m) => m[1]));
const readVars = new Set([...css.matchAll(/var\(\s*(--pool-[a-z0-9-]+)/g)].map((m) => m[1]));

const usedClasses = new Map();
const setVars = new Map();
const note = (map, key, file) => {
  if (!map.has(key)) map.set(key, new Set());
  map.get(key).add(file);
};

for (const file of codeFiles) {
  const src = readFileSync(path.join(root, file), 'utf8');
  // className="..." / className={`...`} / plain 'pool-x' strings in class position.
  for (const m of src.matchAll(/className\s*=\s*(?:"([^"]*)"|\{`([^`]*)`\}|\{'([^']*)'\})/g)) {
    const raw = m[1] ?? m[2] ?? m[3] ?? '';
    // Interpolations produce names no static reader can know; skip the hole, keep the rest.
    // A fragment left dangling on its separator (`pool-delta--${tone}` -> `pool-delta--`)
    // is the hole, not a name, so it is dropped rather than reported as missing.
    for (const cls of raw.replace(/\$\{[^}]*\}/g, ' ').split(/\s+/)) {
      if (cls.startsWith('pool-') && !/[-_]$/.test(cls)) note(usedClasses, cls, file);
    }
  }
  // Classes assembled in arrays or ternaries, e.g. `cond ? 'pool-x--on' : ''`.
  for (const m of src.matchAll(/'(pool-[a-z0-9_-]*--[a-z0-9_-]+)'/g)) note(usedClasses, m[1], file);
  for (const m of src.matchAll(/'(--pool-[a-z0-9-]+)'\s*:/g)) note(setVars, m[1], file);
}

const missingClasses = [...usedClasses.entries()]
  .filter(([cls]) => !definedClasses.has(cls))
  .sort(([a], [b]) => a.localeCompare(b));
const deadVars = [...setVars.entries()]
  .filter(([name]) => !readVars.has(name))
  .sort(([a], [b]) => a.localeCompare(b));
const unreadDefined = [...definedVars].filter((name) => !readVars.has(name)).sort();

// The drift predates this check by a long way: a first run found 68 names, most of them
// page-specific clusters whose intended design is not recoverable from the name alone.
// Inventing rules for those would be worse than leaving them, so the known set is recorded
// and only new names fail. Deleting a line from the baseline is how one gets fixed for good.
const baselinePath = path.join(root, 'scripts/class-drift-baseline.json');
let baseline = { classes: [] };
try {
  baseline = JSON.parse(readFileSync(baselinePath, 'utf8'));
} catch {
  console.error(`missing baseline at ${path.relative(root, baselinePath)} — run with --write-baseline to record the current set`);
}
const known = new Set(baseline.classes || []);
const newDrift = missingClasses.filter(([cls]) => !known.has(cls));
const fixed = [...known].filter((cls) => !usedClasses.has(cls) || definedClasses.has(cls)).sort();

if (process.argv.includes('--write-baseline')) {
  const next = { note: 'Class names emitted by components with no rule in src/styles. Fix one, delete its line.', classes: missingClasses.map(([cls]) => cls) };
  writeFileSync(baselinePath, `${JSON.stringify(next, null, 2)}\n`);
  console.log(`baseline written: ${next.classes.length} known undefined class(es)`);
  process.exit(0);
}

let failed = false;
if (newDrift.length) {
  failed = true;
  console.error(`\n${newDrift.length} NEW class name(s) emitted with no rule in src/styles:\n`);
  for (const [cls, files] of newDrift) {
    console.error(`  ${cls.padEnd(44)} ${[...files].join(', ')}`);
  }
}
if (missingClasses.length) {
  console.log(`known undefined classes: ${missingClasses.length - newDrift.length} of ${known.size} baselined`);
}
if (fixed.length) {
  console.log(`${fixed.length} baselined class(es) no longer drift — delete from the baseline: ${fixed.join(', ')}`);
}
if (deadVars.length) {
  failed = true;
  console.error(`\n${deadVars.length} custom propert(y|ies) set by components that no stylesheet reads:\n`);
  for (const [name, files] of deadVars) {
    console.error(`  ${name.padEnd(44)} ${[...files].join(', ')}`);
  }
}

console.log(`class drift: ${usedClasses.size} emitted, ${definedClasses.size} defined in ${styleFiles.length} stylesheets`);
console.log(`custom properties: ${setVars.size} set from components, ${definedVars.size} declared, ${readVars.size} read`);
if (unreadDefined.length) {
  // Not a failure: a token can be declared as part of a palette and only consumed later.
  console.log(`note: ${unreadDefined.length} declared custom propert(y|ies) are never read via var()`);
}
if (failed) {
  console.error('\nEither add the rule or stop emitting the name — a name with no rule behind it reads as intent that never happens.\n');
  process.exit(1);
}
console.log('no new drift.');
