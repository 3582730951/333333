import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const srcRoot = path.join(root, 'src');

const GLOBALS = new Set([
  'AbortController',
  'Array',
  'Blob',
  'Boolean',
  'CustomEvent',
  'DOMParser',
  'Date',
  'Error',
  'Event',
  'EventTarget',
  'File',
  'FileReader',
  'FormData',
  'JSON',
  'Map',
  'Math',
  'MutationObserver',
  'NaN',
  'Number',
  'Object',
  'Promise',
  'ResizeObserver',
  'Set',
  'String',
  'URL',
  'URLSearchParams',
  'WeakMap',
  'WebSocket',
  'clearInterval',
  'clearTimeout',
  'console',
  'decodeURIComponent',
  'document',
  'encodeURIComponent',
  'fetch',
  'location',
  'localStorage',
  'navigator',
  'parseFloat',
  'parseInt',
  'requestAnimationFrame',
  'sessionStorage',
  'setInterval',
  'setTimeout',
  'undefined',
  'window',
]);

const INTRINSIC_JSX = /^[a-z]/;

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

function hasBinding(pathRef, name) {
  return pathRef.scope.hasBinding(name) || GLOBALS.has(name);
}

function jsxRootName(nameNode) {
  if (!nameNode) return '';
  if (nameNode.type === 'JSXIdentifier') return nameNode.name;
  if (nameNode.type === 'JSXMemberExpression') return jsxRootName(nameNode.object);
  if (nameNode.type === 'JSXNamespacedName') return nameNode.namespace?.name || '';
  return '';
}

function addProblem(problems, file, node, name, kind) {
  const loc = node.loc?.start || { line: 1, column: 0 };
  problems.push({
    file: path.relative(root, file),
    line: loc.line,
    column: loc.column + 1,
    name,
    kind,
  });
}

function checkFile(file) {
  const ast = parseFile(file);
  const problems = [];

  traverse(ast, {
    Identifier(pathRef) {
      if (!pathRef.isReferencedIdentifier()) return;
      const name = pathRef.node.name;
      if (!hasBinding(pathRef, name)) {
        addProblem(problems, file, pathRef.node, name, 'identifier');
      }
    },
    JSXOpeningElement(pathRef) {
      const name = jsxRootName(pathRef.node.name);
      if (!name || INTRINSIC_JSX.test(name)) return;
      if (!hasBinding(pathRef, name)) {
        addProblem(problems, file, pathRef.node.name, name, 'jsx');
      }
    },
  });

  return problems;
}

const problems = listSourceFiles(srcRoot).flatMap(checkFile);

if (problems.length > 0) {
  console.error('Undefined frontend identifiers found:');
  for (const problem of problems) {
    console.error(`${problem.file}:${problem.line}:${problem.column} ${problem.name} (${problem.kind})`);
  }
  process.exit(1);
}

console.log('No undefined frontend identifiers found.');
