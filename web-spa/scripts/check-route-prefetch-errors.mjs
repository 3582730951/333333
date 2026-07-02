import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const appFile = path.join(root, 'src', 'App.jsx');

const ast = parser.parse(fs.readFileSync(appFile, 'utf8'), {
  sourceType: 'module',
  plugins: ['jsx', 'importMeta', 'dynamicImport'],
  errorRecovery: true,
});

const namedPrefetchLoaders = new Map([
  ['ADMIN_PREFETCH_LOADERS', false],
  ['PORTAL_PREFETCH_LOADERS', false],
]);
let reporterLogsClientError = false;
let reporterMarksChunkUpdate = false;
let prefetchCatchUsesReporter = false;

function containsCall(pathRef, name) {
  let found = false;
  pathRef.traverse({
    CallExpression(callPath) {
      if (callPath.node.callee?.name === name) {
        found = true;
        callPath.stop();
      }
    },
  });
  return found;
}

function isNamedLoaderObject(node) {
  if (node?.type !== 'ObjectExpression') return false;
  const keys = new Set(node.properties
    .filter((prop) => prop.type === 'ObjectProperty')
    .map((prop) => prop.key?.name || prop.key?.value));
  return keys.has('name') && keys.has('load');
}

function isNamedPrefetchArray(node) {
  return node?.type === 'ArrayExpression' && node.elements.length > 0 && node.elements.every(isNamedLoaderObject);
}

traverse(ast, {
  VariableDeclarator(pathRef) {
    const name = pathRef.node.id?.name;
    if (name !== 'ADMIN_PREFETCH_LOADERS' && name !== 'PORTAL_PREFETCH_LOADERS') return;
    namedPrefetchLoaders.set(name, isNamedPrefetchArray(pathRef.node.init));
  },
  FunctionDeclaration(pathRef) {
    if (pathRef.node.id?.name !== 'reportRoutePrefetchError') return;
    reporterLogsClientError = containsCall(pathRef, 'reportClientError');
    reporterMarksChunkUpdate =
      containsCall(pathRef, 'isChunkLoadError') &&
      containsCall(pathRef, 'notifyChunkUpdateAvailable');
  },
  CallExpression(pathRef) {
    const node = pathRef.node;
    if (node.callee?.type !== 'MemberExpression' || node.callee.property?.name !== 'catch') return;
    if (containsCall(pathRef, 'reportRoutePrefetchError')) {
      prefetchCatchUsesReporter = true;
    }
  },
});

const failures = [];
if (![...namedPrefetchLoaders.values()].every(Boolean)) {
  failures.push('route prefetch loaders must carry route names for actionable error reports.');
}
if (!reporterLogsClientError) {
  failures.push('route prefetch failures must be reported through reportClientError.');
}
if (!reporterMarksChunkUpdate) {
  failures.push('chunk-load prefetch failures must notify the update banner.');
}
if (!prefetchCatchUsesReporter) {
  failures.push('route prefetch catch handlers must call reportRoutePrefetchError.');
}

if (failures.length > 0) {
  console.error('Route prefetch error handling check failed:');
  for (const failure of failures) console.error(`- ${failure}`);
  process.exit(1);
}

console.log('Route prefetch error handling check passed.');
