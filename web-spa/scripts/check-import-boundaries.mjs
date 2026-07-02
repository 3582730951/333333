import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const srcRoot = path.join(root, 'src');
const heavyChartsModule = path.join(srcRoot, 'components', 'Charts.jsx');
const allowedChartsImporters = new Set([
  path.join(srcRoot, 'components', 'LazyCharts.jsx'),
]);

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

function resolveImport(file, specifier) {
  if (!specifier.startsWith('.')) return '';
  const resolved = path.resolve(path.dirname(file), specifier);
  for (const candidate of [resolved, `${resolved}.js`, `${resolved}.jsx`]) {
    if (fs.existsSync(candidate)) return candidate;
  }
  return resolved;
}

function addProblem(problems, file, node, message) {
  const loc = node.loc?.start || { line: 1, column: 0 };
  problems.push(`${path.relative(root, file)}:${loc.line}:${loc.column + 1} ${message}`);
}

function checkFile(file) {
  const ast = parseFile(file);
  const problems = [];

  const checkSpecifier = (node, specifier) => {
    const resolved = resolveImport(file, specifier);
    if (resolved !== heavyChartsModule || allowedChartsImporters.has(file)) return;
    addProblem(
      problems,
      file,
      node,
      'imports heavy Charts.jsx directly; use components/LazyCharts.jsx for chart UI and lib/chartTheme.js for constants',
    );
  };

  traverse(ast, {
    ImportDeclaration(pathRef) {
      checkSpecifier(pathRef.node.source, pathRef.node.source.value);
    },
    CallExpression(pathRef) {
      if (pathRef.node.callee.type !== 'Import') return;
      const [arg] = pathRef.node.arguments;
      if (arg?.type === 'StringLiteral') checkSpecifier(arg, arg.value);
    },
  });

  return problems;
}

const problems = listSourceFiles(srcRoot).flatMap(checkFile);

if (problems.length > 0) {
  console.error('Frontend import boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Frontend import boundaries check passed.');
