import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const srcRoot = path.join(root, 'src');

const requiredStyleFiles = [
  'tokens.css',
  'base.css',
  'layout.css',
  'components.css',
  'utilities.css',
];
const requiredPoolComponents = [
  'ActionMenu.jsx',
  'Button.jsx',
  'Card.jsx',
  'DataTable.jsx',
  'Dialog.jsx',
  'EmptyState.jsx',
  'Feedback.jsx',
  'Form.jsx',
  'Progress.jsx',
  'Tabs.jsx',
  'index.jsx',
  'icons.jsx',
];
const colorLiteralPattern = /(?:#[0-9a-fA-F]{3,8}\b|rgba?\([^)]*\)|hsla?\([^)]*\))/g;

function listFiles(dir, matcher) {
  const out = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...listFiles(full, matcher));
    else if (matcher(entry.name)) out.push(full);
  }
  return out;
}

function rel(file) {
  return path.relative(root, file).replaceAll(path.sep, '/');
}

function read(file) {
  return fs.existsSync(file) ? fs.readFileSync(file, 'utf8') : '';
}

const failures = [];

const packageSource = read(path.join(root, 'package.json'));
const viteSource = read(path.join(root, 'vite.config.js'));
if (packageSource.includes('@douyinfe/semi')) failures.push('package.json still declares Semi dependencies.');
if (/semi-vite-plugin|SemiPlugin|vendor-semi-ui/.test(viteSource)) failures.push('vite.config.js still includes Semi plugin/chunk configuration.');

for (const file of listFiles(srcRoot, (name) => /\.[cm]?[jt]sx?$|\.css$/.test(name))) {
  const source = read(file);
  if (source.includes('@douyinfe/semi')) failures.push(`${rel(file)} imports or references Semi.`);
}

for (const file of listFiles(srcRoot, (name) => /\.[cm]?[jt]sx?$/.test(name))) {
  const fileRel = rel(file);
  const source = read(file);
  if (/\bPopconfirm\b/.test(source)) failures.push(`${fileRel} still uses Popconfirm; use ActionMenu or ConfirmDialog.`);
  if (/\bSideSheet\b/.test(source)) failures.push(`${fileRel} still uses SideSheet; use Drawer.`);
}

for (const name of requiredStyleFiles) {
  const file = path.join(srcRoot, 'styles', name);
  if (!fs.existsSync(file)) failures.push(`missing design style file src/styles/${name}.`);
}

for (const name of requiredPoolComponents) {
  const file = path.join(srcRoot, 'components', 'pool', name);
  if (!fs.existsSync(file)) failures.push(`missing Pool UI component file src/components/pool/${name}.`);
}

const mainSource = read(path.join(srcRoot, 'main.jsx'));
for (const name of requiredStyleFiles) {
  if (!mainSource.includes(`./styles/${name}`)) failures.push(`main.jsx does not import src/styles/${name}.`);
}

const appSource = `${read(path.join(srcRoot, 'App.tsx'))}\n${read(path.join(srcRoot, 'app', 'useTheme.ts'))}`;
if (!appSource.includes('data-theme')) failures.push('App shell does not set html[data-theme].');
if (appSource.includes('theme-mode')) failures.push('App shell still uses body[theme-mode] instead of html[data-theme].');

for (const file of listFiles(srcRoot, (name) => /\.(?:css|jsx|js|tsx|ts)$/.test(name))) {
  if (rel(file).startsWith('src/assets/')) continue;
  if (rel(file) === 'src/styles/tokens.css') continue;
  const source = read(file);
  for (const match of source.matchAll(colorLiteralPattern)) {
    const line = source.slice(0, match.index).split('\n').length;
    failures.push(`${rel(file)}:${line} hard-coded color literal ${match[0]} outside styles/tokens.css.`);
  }
}

if (failures.length > 0) {
  console.error('Pool UI migration check failed:');
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log('Pool UI migration check passed.');
