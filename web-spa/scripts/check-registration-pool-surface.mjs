import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const root = path.resolve(here, '..');
const read = (rel) => fs.readFileSync(path.join(root, rel), 'utf8');

const failures = [];
const egress = read('src/pages/Egress.jsx');
const registration = read('src/pages/Registration.jsx');
const vite = read('vite.config.js');

for (const [label, source] of [
  ['Egress.jsx', egress],
  ['Registration.jsx', registration],
]) {
  if (/出口池/.test(source)) {
    failures.push(`${label} still exposes the generic 出口池 term; use 注册池/注册代理池 only.`);
  }
}

for (const pattern of [
  /新建出口池/,
  /出口池操作/,
  /purpose:\s*['"]custom['"]/,
  /\{\s*value:\s*['"]custom['"]/,
  /dataSource=\{pools\}/,
  /label:\s*['"]出口池['"]/,
]) {
  if (pattern.test(egress)) {
    failures.push(`Egress.jsx still contains generic pool UI pattern: ${pattern}`);
  }
}

if (!/registrationPools/.test(egress) || !/purpose:\s*['"]registration['"]/.test(egress)) {
  failures.push('Egress.jsx must manage registration pools explicitly with fixed purpose=registration.');
}

if (!/['"]\/auth['"]\s*:/.test(vite)) {
  failures.push('vite.config.js must proxy /auth to the pool server for console auth checks.');
}

if (failures.length) {
  console.error('Registration-pool surface check failed:');
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log('Registration-pool surface check passed.');
