import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';

const traverse = traverseModule.default ?? traverseModule;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const settingsFile = path.join(root, 'src', 'pages', 'SettingsV2.jsx');
const source = fs.readFileSync(settingsFile, 'utf8');
const problems = [];

const ast = parser.parse(source, {
  sourceType: 'module',
  plugins: ['jsx', 'importMeta', 'dynamicImport'],
  errorRecovery: true,
});

function jsxName(node) {
  if (node?.type === 'JSXIdentifier') return node.name;
  if (node?.type === 'JSXMemberExpression') return `${jsxName(node.object)}.${jsxName(node.property)}`;
  return '';
}

function hasJSXAttribute(node, name) {
  return Boolean(getJSXAttribute(node, name));
}

function getJSXAttribute(node, name) {
  return (node.attributes || []).find((attr) => attr.type === 'JSXAttribute' && jsxName(attr.name) === name);
}

function isSettingsFormKey(attr) {
  const expression = attr?.value?.type === 'JSXExpressionContainer' ? attr.value.expression : null;
  return expression?.type === 'CallExpression' && expression.callee?.name === 'settingsFormKey';
}

if (!source.includes("import SettingsTabShell from '../components/SettingsTabShell.jsx';")) {
  problems.push('SettingsV2.jsx must use the shared SettingsTabShell wrapper.');
}

if (/loading\s*&&\s*!lastRefresh/.test(source)) {
  problems.push('SettingsV2.jsx must not duplicate first-load spinner checks inside each tab.');
}

if (/\bSavedDiffPanel\b/.test(source) || /\bSettingsErrorBanner\b/.test(source)) {
  problems.push('SettingsV2.jsx must keep diff and settings-error rendering in SettingsTabShell.');
}

if (/onUndo=\{\s*\(\)\s*=>\s*\{\s*\}\s*\}/.test(source)) {
  problems.push('SettingsV2.jsx must not render a fake undo action for tabs without undo support.');
}

if (!/\bfunction\s+ConfigFieldRow\b/.test(source)) {
  problems.push('SettingsV2.jsx must keep config field rows in the ConfigFieldRow component.');
}

if (/className="pool-settings-row"\s+style=/.test(source)) {
  problems.push('SettingsV2.jsx must keep config row layout in theme.css, not inline JSX styles.');
}

if (!/\bfunction\s+settingsFormKey\b/.test(source)) {
  problems.push('SettingsV2.jsx must use settingsFormKey for async Form initValues remount keys.');
}

traverse(ast, {
  JSXOpeningElement(pathRef) {
    const node = pathRef.node;
    if (jsxName(node.name) !== 'Form') return;
    if (!hasJSXAttribute(node, 'initValues')) return;
    const keyAttr = getJSXAttribute(node, 'key');
    if (isSettingsFormKey(keyAttr)) return;
    const loc = node.loc?.start || { line: 1, column: 0 };
    problems.push(`SettingsV2.jsx:${loc.line}:${loc.column + 1} Form with async initValues must use key={settingsFormKey(...)}.`);
  },
});

if (problems.length > 0) {
  console.error('Settings shell boundary check failed:');
  for (const problem of problems) console.error(`- ${problem}`);
  process.exit(1);
}

console.log('Settings shell boundary check passed.');
