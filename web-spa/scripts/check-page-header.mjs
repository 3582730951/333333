import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const problems = [];

const headerFile = path.join(root, 'src', 'components', 'PageHeader.jsx');
const headerSource = fs.readFileSync(headerFile, 'utf8');
for (const required of ['pool-pagehead-copy', 'pool-page-actions']) {
  if (!headerSource.includes(required)) {
    problems.push(`PageHeader.jsx must centralize page header layout with ${required}.`);
  }
}
if (!/className="actions pool-page-actions"/.test(headerSource)) {
  problems.push('PageHeader.jsx actions wrapper must keep both legacy actions and pool-page-actions classes.');
}

const themeFile = path.join(root, 'src', 'theme.css');
const themeSource = fs.readFileSync(themeFile, 'utf8');
for (const required of [
  '.pool-pagehead-copy',
  '.pool-page-actions',
  '.pool-page-actions > *',
  '@media (max-width: 520px)',
]) {
  if (!themeSource.includes(required)) {
    problems.push(`theme.css must include responsive PageHeader action layout rule ${required}.`);
  }
}

if (problems.length > 0) {
  console.error('Page header boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Page header boundary check passed.');
