import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workspaceRoot = path.resolve(root, '..');
const srcRoot = path.join(root, 'src');
const appFile = path.join(srcRoot, 'App.jsx');
const apiFile = path.join(srcRoot, 'api.js');
const i18nFile = path.join(srcRoot, 'lib', 'i18n.js');

const expectedAdminRoutes = [
  '/',
  '/accounts',
  '/groups',
  '/egress',
  '/providers',
  '/registration',
  '/lifecycle',
  '/gopay',
  '/usage',
  '/quota',
  '/system',
  '/cf-events',
  '/audit',
  '/keys',
  '/users',
  '/thinking',
  '/moderation',
  '/settings-v2',
  '/automation',
  '/settings',
];
const expectedPortalRoutes = ['/portal', '/portal/keys', '/portal/profile'];
const expectedStorageKeys = ['pool_admin_token', 'pool_theme', 'pool_locale'];

function listFiles(dir, matcher) {
  const out = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...listFiles(full, matcher));
    else if (matcher(entry.name)) out.push(full);
  }
  return out;
}

function relative(file) {
  return path.relative(workspaceRoot, file).replaceAll(path.sep, '/');
}

function read(file) {
  return fs.readFileSync(file, 'utf8');
}

function parseSource(file) {
  return parser.parse(read(file), {
    sourceType: 'module',
    plugins: ['jsx', 'importMeta', 'dynamicImport'],
    errorRecovery: true,
  });
}

function collectSemiImports() {
  const files = [
    ...listFiles(srcRoot, (name) => /\.[cm]?jsx?$/.test(name)),
    path.join(root, 'vite.config.js'),
    path.join(root, 'package.json'),
  ].filter((file) => fs.existsSync(file));
  const imports = [];
  for (const file of files) {
    const source = read(file);
    const lines = source.split('\n');
    lines.forEach((line, index) => {
      if (/@douyinfe\/semi|semi-ui|semi-icons|semi-vite-plugin/.test(line)) {
        imports.push({ file: relative(file), line: index + 1, text: line.trim() });
      }
    });
  }
  return imports;
}

function collectRoutePaths() {
  const routes = new Set();
  const ast = parseSource(appFile);
  traverse(ast, {
    ObjectProperty(pathRef) {
      const { node } = pathRef;
      const key = node.key;
      const value = node.value;
      const isPathKey = key.type === 'Identifier' && (key.name === 'path' || key.name === 'navPath' || key.name === 'redirectTo');
      if (isPathKey && value.type === 'StringLiteral') routes.add(value.value);
    },
  });
  return [...routes].sort();
}

function collectApiCalls() {
  const files = listFiles(srcRoot, (name) => /\.[cm]?jsx?$/.test(name));
  const calls = [];
  for (const file of files) {
    const ast = parseSource(file);
    traverse(ast, {
      CallExpression(pathRef) {
        const { node } = pathRef;
        const callee = node.callee;
        const direct = callee.type === 'Identifier' && ['get', 'post', 'put', 'patch', 'del'].includes(callee.name);
        if (!direct) return;
        const first = node.arguments[0];
        let endpoint = '';
        if (first?.type === 'StringLiteral') endpoint = first.value;
        if (first?.type === 'TemplateLiteral') endpoint = first.quasis.map((q) => q.value.raw).join('${}');
        if (endpoint.startsWith('/')) calls.push({ file: relative(file), method: callee.name, endpoint });
      },
    });
  }
  return calls.sort((a, b) => `${a.file}:${a.endpoint}`.localeCompare(`${b.file}:${b.endpoint}`));
}

function collectStorageKeys() {
  const files = listFiles(srcRoot, (name) => /\.[cm]?jsx?$/.test(name));
  const keys = new Set();
  for (const file of files) {
    const source = read(file);
    for (const match of source.matchAll(/['"`](pool_[a-z0-9_:-]+)['"`]/gi)) {
      keys.add(match[1]);
    }
  }
  return [...keys].sort();
}

function collectComponentUsage() {
  const buckets = {
    modal: /\b(?:Modal|SideSheet|Drawer|ConfirmDialog|Popconfirm|AlertDialog)\b/g,
    form: /\bForm\./g,
    table: /\b(?:Table|ResourceTable|DataTable)\b/g,
    toast: /\bToast\./g,
    popover: /\b(?:Popover|Popconfirm|DropdownMenu|ActionMenu|Tooltip)\b/g,
  };
  const files = listFiles(srcRoot, (name) => /\.[cm]?jsx?$/.test(name));
  const usage = Object.fromEntries(Object.keys(buckets).map((key) => [key, []]));
  for (const file of files) {
    const source = read(file);
    for (const [kind, pattern] of Object.entries(buckets)) {
      const count = [...source.matchAll(pattern)].length;
      if (count > 0) usage[kind].push({ file: relative(file), count });
    }
  }
  return usage;
}

function assertAnchors({ routes, storageKeys }) {
  const failures = [];
  for (const route of [...expectedAdminRoutes, ...expectedPortalRoutes]) {
    if (!routes.includes(route)) failures.push(`missing route anchor ${route}`);
  }
  for (const key of expectedStorageKeys) {
    if (!storageKeys.includes(key)) failures.push(`missing storage key ${key}`);
  }
  const apiSource = read(apiFile);
  const appSource = read(appFile);
  const i18nSource = read(i18nFile);
  const requiredSources = [
    { label: 'auth probe /auth/me', ok: apiSource.includes("'/auth/me'") },
    { label: 'session logout /auth/logout', ok: apiSource.includes("'/auth/logout'") },
    { label: 'admin token bearer header', ok: apiSource.includes('Authorization') && apiSource.includes('Bearer') },
    { label: 'unauthorized browser event', ok: apiSource.includes('pool-unauthorized') && appSource.includes('pool-unauthorized') },
    { label: 'theme persistence key', ok: appSource.includes('pool_theme') },
    { label: 'locale persistence key', ok: i18nSource.includes('pool_locale') },
  ];
  for (const item of requiredSources) {
    if (!item.ok) failures.push(`missing behavior anchor: ${item.label}`);
  }
  return failures;
}

const inventory = {
  semiImports: collectSemiImports(),
  routes: collectRoutePaths(),
  apiCalls: collectApiCalls(),
  storageKeys: collectStorageKeys(),
  componentUsage: collectComponentUsage(),
};
const failures = assertAnchors({ routes: inventory.routes, storageKeys: inventory.storageKeys });

if (failures.length > 0) {
  console.error('UI inventory check failed:');
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log('UI inventory check passed.');
console.log(`- Semi references: ${inventory.semiImports.length}`);
console.log(`- Routes: ${inventory.routes.length}`);
console.log(`- API call sites: ${inventory.apiCalls.length}`);
console.log(`- Storage keys: ${inventory.storageKeys.join(', ')}`);
for (const [kind, entries] of Object.entries(inventory.componentUsage)) {
  const count = entries.reduce((sum, item) => sum + item.count, 0);
  console.log(`- ${kind}: ${count} references across ${entries.length} files`);
}
