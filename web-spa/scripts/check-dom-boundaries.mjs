import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const checkedRoots = [
  path.join(root, 'src', 'pages'),
  path.join(root, 'src', 'components'),
];

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
    plugins: ['jsx', 'importMeta', 'dynamicImport'],
    errorRecovery: true,
  });
}

function addProblem(problems, file, node, message) {
  const loc = node.loc?.start || { line: 1, column: 0 };
  problems.push(`${path.relative(root, file)}:${loc.line}:${loc.column + 1} ${message}`);
}

function isDocumentQuery(node) {
  return (
    node?.callee?.type === 'MemberExpression' &&
    node.callee.object?.name === 'document' &&
    ['querySelector', 'querySelectorAll', 'getElementById'].includes(node.callee.property?.name)
  );
}

function isSubmitEventDispatch(node) {
  if (node?.callee?.type !== 'MemberExpression' || node.callee.property?.name !== 'dispatchEvent') return false;
  const [arg] = node.arguments || [];
  if (arg?.type !== 'NewExpression' || arg.callee?.name !== 'Event') return false;
  const [name] = arg.arguments || [];
  return name?.type === 'StringLiteral' && name.value === 'submit';
}

function checkFile(file) {
  const ast = parseFile(file);
  const problems = [];

  traverse(ast, {
    CallExpression(pathRef) {
      if (isDocumentQuery(pathRef.node)) {
        addProblem(problems, file, pathRef.node, 'avoid document DOM queries in React pages/components; use refs or component APIs');
      }
      if (isSubmitEventDispatch(pathRef.node)) {
        addProblem(problems, file, pathRef.node, 'do not dispatch synthetic submit events; use the Form API/ref');
      }
    },
  });

  return problems;
}

const problems = checkedRoots.flatMap((dir) => listSourceFiles(dir).flatMap(checkFile));

if (problems.length > 0) {
  console.error('Frontend DOM boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Frontend DOM boundary check passed.');
