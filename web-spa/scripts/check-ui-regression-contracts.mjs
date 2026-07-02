import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const src = path.join(root, 'src');
const problems = [];

function read(rel) {
  return fs.readFileSync(path.join(src, rel), 'utf8');
}

const button = read('components/pool/Button.jsx');
if (!/forwardRef/.test(button) || !/ref=/.test(button)) {
  problems.push('Pool Button must use React.forwardRef and pass ref to the native button for Radix asChild triggers.');
}

const feedback = read('components/pool/Feedback.jsx');
if (!/function Space\([^)]*wrap/.test(feedback) || /<span[^>]*\{\.\.\.props\}[^>]*>/.test(feedback) && !/const domProps/.test(feedback)) {
  problems.push('Space must consume wrap/spacing props instead of forwarding non-DOM props to <span>.');
}

const dataTable = read('components/pool/DataTable.jsx');
if (!/rowKey\(row,\s*index\)\s*\?\?/.test(dataTable) && !/rowKey\(row,\s*index\)[\s\S]{0,80}index/.test(dataTable)) {
  problems.push('DataTable must fall back to the row index when a function rowKey returns nullish.');
}
if (!/data-label=/.test(dataTable)) {
  problems.push('DataTable cells must expose data-label for mobile card layout.');
}
if (!/pool-table--cards/.test(dataTable)) {
  problems.push('DataTable must mark tables with a mobile card-layout class.');
}

const componentsCss = read('styles/components.css');
if (!/\.pool-table--cards/.test(componentsCss) || !/data-label/.test(componentsCss)) {
  problems.push('components.css must implement the DataTable mobile card layout.');
}

if (problems.length > 0) {
  console.error('UI regression contract check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('UI regression contract check passed.');
