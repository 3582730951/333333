#!/usr/bin/env node
/**
 * Aurora P0 verification-coverage audit.
 * Extracts canonical routes and hard-coded script route/file lists with Babel AST,
 * then reports the routes a "passing" script never touches.
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const routeFile = path.join(root, 'src', 'app', 'routeDefinitions.ts');
const scripts = [
  'scripts/check-visual-smoke.mjs',
  'scripts/capture-ui-review.mjs',
  'scripts/check-layout-collisions.mjs',
  'scripts/measure-edge-proximity.mjs',
  'scripts/check-ui-inventory.mjs',
  'scripts/check-resource-table.mjs',
];

function readArg(name) {
  const index = process.argv.indexOf(name);
  return index === -1 ? null : process.argv[index + 1] || null;
}

function parse(file, plugins = ['jsx', 'typescript']) {
  return parser.parse(fs.readFileSync(file, 'utf8'), { sourceType: 'unambiguous', plugins });
}

function stringValue(node) {
  return node?.type === 'StringLiteral' ? node.value : null;
}

function prop(object, name) {
  if (object?.type !== 'ObjectExpression') return null;
  return object.properties.find((item) => item.type === 'ObjectProperty'
    && (item.key.type === 'Identifier' ? item.key.name : stringValue(item.key)) === name) || null;
}

function importPath(loader) {
  const body = loader?.type === 'ArrowFunctionExpression' ? loader.body : null;
  if (body?.type === 'ImportExpression') return stringValue(body.source);
  if (body?.type === 'CallExpression' && body.callee.type === 'Import') return stringValue(body.arguments[0]);
  return null;
}

const declared = [];
traverse(parse(routeFile), {
  ObjectExpression(pathRef) {
    const pathValue = stringValue(prop(pathRef.node, 'path')?.value);
    const role = stringValue(prop(pathRef.node, 'role')?.value);
    const component = importPath(prop(pathRef.node, 'lazyLoader')?.value);
    if (pathValue && role && component) declared.push({ path: pathValue, role, component: component.split('/').at(-1) });
  },
});
const declaredPaths = new Set(declared.map((route) => route.path));
const componentToPaths = new Map();
for (const route of declared) {
  const values = componentToPaths.get(route.component) || [];
  values.push(route.path);
  componentToPaths.set(route.component, values);
}

function collectStrings(file) {
  const values = [];
  const functions = new Set();
  traverse(parse(file, ['jsx', 'typescript', 'dynamicImport']), {
    StringLiteral(pathRef) { values.push({ value: pathRef.node.value, line: pathRef.node.loc.start.line }); },
    FunctionDeclaration(pathRef) { if (pathRef.node.id?.name) functions.add(pathRef.node.id.name); },
  });
  return { values, functions };
}

const results = scripts.map((relative) => {
  const file = path.join(root, relative);
  const { values, functions } = collectStrings(file);
  const directRoutes = [...new Set(values.map((item) => item.value).filter((value) => declaredPaths.has(value)))].sort();
  const pageFiles = [...new Set(values.map((item) => item.value).filter((value) => componentToPaths.has(value)))];
  const fileRoutes = pageFiles.flatMap((name) => componentToPaths.get(name) || []);
  const covered = [...new Set([...directRoutes, ...fileRoutes])].sort();
  return {
    file: relative,
    declaredRoutes: declared.length,
    coveredRoutes: covered,
    coveredCount: covered.length,
    missingRoutes: declared.map((route) => route.path).filter((route) => !covered.includes(route)).sort(),
    routeCoverageAssertion: functions.has('assertRouteCoverage'),
    directRouteList: directRoutes.length > 0,
    componentFileList: pageFiles,
  };
});

const output = { generatedAt: new Date().toISOString(), declared, results };
const out = readArg('--out');
const text = `${JSON.stringify(output, null, 2)}\n`;
if (out) {
  fs.mkdirSync(path.dirname(path.resolve(out)), { recursive: true });
  fs.writeFileSync(out, text);
} else process.stdout.write(text);
console.error(`Aurora P0 gate coverage: ${declared.length} declared routes; output ${out || 'stdout'}`);
