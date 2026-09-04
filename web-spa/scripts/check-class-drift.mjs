#!/usr/bin/env node
// Three bugs this pass came from the same crack: a class or custom property that one layer
// writes and no other layer reads. `pool-text-clamp--multi` had no CSS, so `lines={2}` was
// silently a no-op; `pool-table-layout-fit` had none either, and twelve pages passed it
// expecting behaviour. Neither shows up in a typecheck, a unit test, or a screenshot that
// happens not to exercise the case. This finds them by comparing what the components emit
// against what the stylesheets define.
import { readFileSync, readdirSync, writeFileSync } from 'node:fs';
import path from 'node:path';

import { OWNED_PREFIXES, collectOwnedClassesFromJsx } from './lib/class-drift-jsx.mjs';

const root = path.resolve(import.meta.dirname, '..');
const list = (dir, matcher) => {
  const found = [];
  const walk = (relative) => {
    for (const entry of readdirSync(path.join(root, relative), { withFileTypes: true })) {
      const child = path.join(relative, entry.name);
      if (entry.isDirectory()) walk(child);
      else if (matcher(entry.name)) found.push(child);
    }
  };
  walk(dir);
  return found;
};

const styleFiles = list('src/styles', (name) => name.endsWith('.css'));
const codeFiles = [
  ...list('src', (name) => name.endsWith('.jsx')),
  ...list('src', (name) => name.endsWith('.tsx')),
];

// The prefixes this app owns. `pool-` was the only one checked for a long time, which left whole
// families unwatched: `upstream-rule-main` and `public-chat-admin-page` were both emitted with no
// rule behind them and neither ever failed a gate, because neither starts with `pool-`. Measured
// scope when this widened: 3 undefined names outside `pool-`, so the miss was narrow -- but narrow
// is not the same as covered, and the whole point of this check is that a name with no rule is
// invisible to typecheck, unit tests, and any screenshot that happens not to exercise it.
//
// Prefix-scoped rather than "every class we emit": the Semi Design components bring their own
// `semi-*` classes whose rules live in node_modules, and matching those would report the entire
// vendor surface as drift.
const CLASS_RE = new RegExp(`\\.((?:${OWNED_PREFIXES.join('|')})[a-z0-9_-]*)`, 'g');

// Comments are stripped before anything is read as a selector. A comment that discusses a class by
// name -- `.upstream-rule-main is deliberately not styled here` -- otherwise registers as a
// definition and silently cancels the drift report for that name. That happened on the first run of
// the widened check above: the one class it was written to catch came back clean because the comment
// explaining why it has no rule was itself counted as the rule. A gate whose own documentation
// disables it is worse than no gate, since it reports success.
const stripComments = (text) => text.replace(/\/\*[\s\S]*?\*\//g, ' ');

const css = stripComments(styleFiles.map((f) => readFileSync(path.join(root, f), 'utf8')).join('\n'));
// Class names as they appear in selectors, and custom properties as they are declared.
const definedClasses = new Set([...css.matchAll(CLASS_RE)].map((m) => m[1]));
const definedVars = new Set([...css.matchAll(/(--pool-[a-z0-9-]+)\s*:/g)].map((m) => m[1]));
const readVars = new Set([...css.matchAll(/var\(\s*(--pool-[a-z0-9-]+)/g)].map((m) => m[1]));

const usedClasses = new Map();
const setVars = new Map();
const indeterminateClasses = [];
const note = (map, key, file) => {
  if (!map.has(key)) map.set(key, new Set());
  map.get(key).add(file);
};

for (const file of codeFiles) {
  const src = readFileSync(path.join(root, file), 'utf8');
  // JSX AST traversal covers direct strings, templates (including literal template holes),
  // conditional/logical values, arrays, and clsx-style arguments. Unlike the old source regex,
  // a naked `className={condition ? 'pool-a' : 'pool-b'}` reaches this path too.
  const usage = collectOwnedClassesFromJsx(src, { file });
  for (const cls of usage.classes) note(usedClasses, cls, file);
  indeterminateClasses.push(...usage.indeterminate);
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
console.log(`不可静态判定的 className 表达式: ${indeterminateClasses.length}`);
for (const item of indeterminateClasses) {
  console.log(`  ${item.file}:${item.line}:${item.column} ${item.expression} — ${item.reason}`);
}
if (unreadDefined.length) {
  // Not a failure: a token can be declared as part of a palette and only consumed later.
  console.log(`note: ${unreadDefined.length} declared custom propert(y|ies) are never read via var()`);
}
if (failed) {
  console.error('\nEither add the rule or stop emitting the name — a name with no rule behind it reads as intent that never happens.\n');
  process.exit(1);
}
console.log('no new drift.');
