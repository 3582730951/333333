import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const dashboardFile = path.join(root, 'src', 'features', 'observability', 'api', 'dashboard.ts');
const source = fs.readFileSync(dashboardFile, 'utf8');
const problems = [];

const ast = parser.parse(source, {
  sourceType: 'module',
  plugins: ['jsx', 'typescript', 'importMeta'],
  errorRecovery: true,
});

const loaderNodes = new Map();

traverse(ast, {
  FunctionDeclaration(pathRef) {
    const name = pathRef.node.id?.name;
    if (name === 'fetchDashboardCore' || name === 'fetchDashboardSecondary') loaderNodes.set(name, pathRef.node);
  },
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
    problems.push(`dashboard.ts must define ${name}.`);
    return;
  }
  const identifiers = new Set();
  traverse(ast, {
    Identifier(pathRef) {
      if (pathRef.findParent((parent) => parent.node === loader)) identifiers.add(pathRef.node.name);
    },
  });
  for (const helper of required) {
    if (!identifiers.has(helper)) problems.push(`${name} must call ${helper}.`);
  }
  for (const helper of forbidden) {
    if (identifiers.has(helper)) problems.push(`${name} must not call secondary helper ${helper}.`);
  }
}

assertLoader('fetchDashboardCore', {
  required: ['fetchHealth', 'fetchAccountSummary', 'fetchDashboardTimeseries'],
  forbidden: ['fetchRegistrationStats', 'fetchDashboardSystem', 'fetchDashboardModels', 'fetchDashboardCache'],
});

assertLoader('fetchDashboardSecondary', {
  required: ['fetchRegistrationStats', 'fetchDashboardSystem', 'fetchDashboardModels', 'fetchDashboardCache'],
});

if (/const\s+fetchDashboard\s*=/.test(source)) {
  problems.push('dashboard.ts must keep core and secondary loaders split instead of a single fetchDashboard aggregator.');
}

for (const endpoint of ['/healthz', '/admin/accounts/summary', '/admin/usage/timeseries', '/admin/register/stats', '/admin/system', '/admin/usage/by-model', '/admin/usage/cache']) {
  if (!source.includes(endpoint)) problems.push(`dashboard.ts must retain endpoint ${endpoint}.`);
}

if (problems.length > 0) {
  console.error('Dashboard resource boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Dashboard resource boundary check passed.');
