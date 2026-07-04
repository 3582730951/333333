import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const dashboardFile = path.join(root, 'src', 'pages', 'Dashboard.jsx');
const source = fs.readFileSync(dashboardFile, 'utf8');
const problems = [];

const ast = parser.parse(source, {
  sourceType: 'module',
  plugins: ['jsx', 'importMeta'],
  errorRecovery: true,
});

const loaderNodes = new Map();

traverse(ast, {
  VariableDeclarator(pathRef) {
    const name = pathRef.node.id?.name;
    if (name === 'fetchDashboardCore' || name === 'fetchDashboardSecondary') {
      loaderNodes.set(name, pathRef.node.init);
    }
  },
});

function collectStringValues(node, values = []) {
  if (!node || typeof node !== 'object') return values;
  if (Array.isArray(node)) {
    node.forEach((item) => collectStringValues(item, values));
    return values;
  }
  if (node.type === 'StringLiteral' || (node.type === 'Literal' && typeof node.value === 'string')) {
    values.push(node.value);
  }
  for (const [key, value] of Object.entries(node)) {
    if (key === 'loc' || key === 'start' || key === 'end' || key === 'extra') continue;
    collectStringValues(value, values);
  }
  return values;
}

function assertLoader(name, { required = [], forbidden = [] }) {
  const loader = loaderNodes.get(name);
  if (!loader) {
    problems.push(`Dashboard.jsx must define ${name}.`);
    return;
  }
  const strings = new Set(collectStringValues(loader));
  for (const endpoint of required) {
    if (!strings.has(endpoint)) problems.push(`${name} must load ${endpoint}.`);
  }
  for (const endpoint of forbidden) {
    if (strings.has(endpoint)) problems.push(`${name} must not load secondary endpoint ${endpoint}.`);
  }
}

assertLoader('fetchDashboardCore', {
  required: ['/healthz', '/admin/accounts/summary', '/admin/usage/timeseries'],
  forbidden: ['/admin/accounts', '/admin/register/stats', '/admin/system', '/admin/usage/by-model'],
});

assertLoader('fetchDashboardSecondary', {
  required: ['/admin/register/stats', '/admin/system', '/admin/usage/by-model', '/admin/usage/cache'],
});

if (/const\s+fetchDashboard\s*=/.test(source)) {
  problems.push('Dashboard.jsx must keep core and secondary loaders split instead of a single fetchDashboard aggregator.');
}

if (problems.length > 0) {
  console.error('Dashboard resource boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Dashboard resource boundary check passed.');
