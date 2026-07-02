import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const source = fs.readFileSync(path.join(root, 'src', 'components', 'AccountDrawer.jsx'), 'utf8');
const problems = [];

if (!source.includes('/admin/audit')) {
  problems.push('AccountDrawer.jsx must keep loading recent audit rows for the account detail view.');
}

if (!/get\(\s*['"]\/admin\/audit['"]\s*,\s*\{[^}]*\baccount_id\b[^}]*\}/s.test(source)) {
  problems.push('AccountDrawer.jsx must scope /admin/audit with account_id instead of loading the global audit stream.');
}

if (/get\(\s*['"]\/admin\/audit['"]\s*,\s*\{\s*limit\s*:\s*(?:300|500)\b/s.test(source)) {
  problems.push('AccountDrawer.jsx must not load hundreds of global audit rows for a drawer.');
}

if (/values\.audit[\s\S]{0,240}\.filter\(\s*\(?\s*\w+\s*\)?\s*=>[\s\S]{0,160}account_id/s.test(source)) {
  problems.push('AccountDrawer.jsx must not client-filter account audit rows after a global audit fetch.');
}

if (/\/egress-binding/.test(source)) {
  problems.push('AccountDrawer.jsx must reuse row-level egress_binding instead of refetching the account binding on open.');
}

if (!/account\.egress_binding/.test(source)) {
  problems.push('AccountDrawer.jsx must render the egress binding from the account row payload.');
}

if (problems.length > 0) {
  console.error('Account drawer resource boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Account drawer resource boundary check passed.');
