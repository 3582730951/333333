import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const pagesRoot = path.join(root, 'src', 'pages');

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

function isSetIntervalCall(node) {
  if (node?.callee?.type === 'Identifier') return node.callee.name === 'setInterval';
  if (node?.callee?.type !== 'MemberExpression') return false;
  const objectName = node.callee.object?.name;
  const propertyName = node.callee.property?.name;
  return objectName === 'window' && propertyName === 'setInterval';
}

function checkFile(file) {
  const ast = parseFile(file);
  const problems = [];
  traverse(ast, {
    CallExpression(pathRef) {
      if (!isSetIntervalCall(pathRef.node)) return;
      const loc = pathRef.node.loc?.start || { line: 1, column: 0 };
      problems.push(`${path.relative(root, file)}:${loc.line}:${loc.column + 1} use hooks/useVisibleInterval.js instead of page-local setInterval polling`);
    },
  });
  return problems;
}

const problems = listSourceFiles(pagesRoot).flatMap(checkFile);

if (problems.length > 0) {
  console.error('Page polling boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Page polling boundary check passed.');
