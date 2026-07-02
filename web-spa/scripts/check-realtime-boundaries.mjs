import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const srcRoot = path.join(root, 'src');
const realtimeModule = path.join(srcRoot, 'lib', 'browserRealtime.js');

function listSourceFiles(dir) {
  const files = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const fullPath = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      files.push(...listSourceFiles(fullPath));
    } else if (/\.[cm]?jsx?$/.test(entry.name)) {
      files.push(fullPath);
    }
  }
  return files;
}

function parseFile(file) {
  return parser.parse(fs.readFileSync(file, 'utf8'), {
    sourceType: 'module',
    plugins: ['jsx', 'importMeta'],
    errorRecovery: true,
  });
}

function checkFile(file) {
  const ast = parseFile(file);
  const problems = [];

  traverse(ast, {
    NewExpression(pathRef) {
      if (pathRef.node.callee?.type !== 'Identifier' || pathRef.node.callee.name !== 'WebSocket') return;
      if (file === realtimeModule) return;
      const loc = pathRef.node.loc?.start || { line: 1, column: 0 };
      problems.push(`${path.relative(root, file)}:${loc.line}:${loc.column + 1} use browserRealtime.createWebSocket instead of new WebSocket`);
    },
    MemberExpression(pathRef) {
      if (pathRef.node.object?.type !== 'Identifier' || pathRef.node.object.name !== 'WebSocket') return;
      if (file === realtimeModule) return;
      const loc = pathRef.node.loc?.start || { line: 1, column: 0 };
      problems.push(`${path.relative(root, file)}:${loc.line}:${loc.column + 1} use browserRealtime helpers instead of WebSocket state constants`);
    },
    Identifier(pathRef) {
      if (pathRef.node.name !== 'WebSocket') return;
      if (file === realtimeModule) return;
      const parent = pathRef.parentPath?.node;
      if (parent?.type === 'NewExpression' && parent.callee === pathRef.node) return;
      if (parent?.type === 'MemberExpression' && parent.object === pathRef.node) return;
      const loc = pathRef.node.loc?.start || { line: 1, column: 0 };
      problems.push(`${path.relative(root, file)}:${loc.line}:${loc.column + 1} use browserRealtime helpers instead of WebSocket global`);
    },
  });

  return problems;
}

const problems = listSourceFiles(srcRoot).flatMap(checkFile);

if (problems.length > 0) {
  console.error('Realtime boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Realtime boundary check passed.');
