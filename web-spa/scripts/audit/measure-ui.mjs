#!/usr/bin/env node
/**
 * Aurora P0 static UI measurement.
 *
 * This intentionally uses Babel ASTs for JS/TS/JSX/TSX and PostCSS ASTs for
 * stylesheets. It never obtains a count by grepping source text, so locations
 * and totals remain reproducible when the source changes.
 *
 * Usage:
 *   node scripts/audit/measure-ui.mjs --out /tmp/aurora-p0-static.json
 */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';
import postcss from 'postcss';

const traverse = traverseModule.default;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const srcRoot = path.join(root, 'src');
const stylesRoot = path.join(srcRoot, 'styles');
const SOURCE_EXTENSIONS = new Set(['.js', '.jsx', '.ts', '.tsx']);
const CSS_EXTENSIONS = new Set(['.css']);
const HAN = /\p{Script=Han}/u;
const HAN_GLOBAL = /\p{Script=Han}/gu;
const PX_GLOBAL = /(-?(?:\d*\.\d+|\d+))px\b/g;

function walk(dir, extensions) {
  const files = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const absolute = path.join(dir, entry.name);
    if (entry.isDirectory()) files.push(...walk(absolute, extensions));
    else if (extensions.has(path.extname(entry.name))) files.push(absolute);
  }
  return files.sort();
}

function rel(file) {
  return path.relative(root, file).replaceAll(path.sep, '/');
}

function loc(file, nodeOrLine) {
  const line = typeof nodeOrLine === 'number' ? nodeOrLine : nodeOrLine?.loc?.start?.line;
  return `${rel(file)}:${line || 1}`;
}

function parseSource(file) {
  try {
    return parser.parse(fs.readFileSync(file, 'utf8'), {
      sourceType: 'unambiguous',
      dts: file.endsWith('.d.ts'),
      plugins: ['jsx', 'typescript', 'classProperties', 'classPrivateProperties', 'topLevelAwait', 'importAttributes'],
    });
  } catch (error) {
    error.message = `${rel(file)}: ${error.message}`;
    throw error;
  }
}

function stringValue(node) {
  if (!node) return null;
  if (node.type === 'StringLiteral' || node.type === 'DirectiveLiteral') return node.value;
  if (node.type === 'TemplateElement') return node.value.cooked ?? node.value.raw;
  return null;
}

function objectProperty(object, key) {
  if (!object || object.type !== 'ObjectExpression') return null;
  for (const property of object.properties) {
    if (property.type !== 'ObjectProperty') continue;
    const name = property.key.type === 'Identifier' ? property.key.name : stringValue(property.key);
    if (name === key) return property;
  }
  return null;
}

function addLocation(map, key, value) {
  const entry = map.get(key) || { value: key, count: 0, locations: [] };
  entry.count += 1;
  if (entry.locations.length < 12) entry.locations.push(value);
  map.set(key, entry);
}

function addFileCount(map, file, amount = 1) {
  map.set(file, (map.get(file) || 0) + amount);
}

function topFileCounts(map, limit = 20) {
  return [...map.entries()]
    .map(([file, count]) => ({ file, count }))
    .sort((a, b) => b.count - a.count || a.file.localeCompare(b.file))
    .slice(0, limit);
}

function compactEntries(map, limit = Infinity) {
  return [...map.values()]
    .sort((a, b) => b.count - a.count || String(a.value).localeCompare(String(b.value)))
    .slice(0, limit);
}

function literalNumber(node) {
  if (!node) return null;
  if (node.type === 'NumericLiteral') return String(node.value);
  if (node.type === 'StringLiteral') return node.value;
  return null;
}

function routeValue(property) {
  const value = property?.value;
  if (!value) return null;
  if (value.type === 'StringLiteral') return value.value;
  if (value.type === 'BooleanLiteral') return value.value;
  return null;
}

function propertyByName(node, name) {
  if (!node || node.type !== 'ObjectExpression') return null;
  return node.properties.find((property) => {
    if (property.type !== 'ObjectProperty') return false;
    const key = property.key.type === 'Identifier' ? property.key.name : stringValue(property.key);
    return key === name;
  }) || null;
}

function isRouteObject(node) {
  return node?.type === 'ObjectExpression'
    && Boolean(propertyByName(node, 'path'))
    && Boolean(propertyByName(node, 'role'))
    && Boolean(propertyByName(node, 'lazyLoader'));
}

function importTarget(node) {
  if (!node) return null;
  if (node.type === 'ImportExpression') return stringValue(node.source);
  if (node.type === 'CallExpression' && node.callee.type === 'Import') return stringValue(node.arguments[0]);
  return null;
}

function listPixelValues(value) {
  const result = [];
  for (const match of value.matchAll(PX_GLOBAL)) result.push(Number(match[1]));
  return result;
}

function propertyIsSpacing(prop) {
  return /^(?:margin(?:-|$)|padding(?:-|$)|gap$|row-gap$|column-gap$|inset(?:-|$)|top$|right$|bottom$|left$|width$|height$|min-|max-|flex-basis$|grid-template-columns$|grid-template-rows$|border(?:-(?:top|right|bottom|left))?-width$)/.test(prop);
}

function propertyIsRhythm(prop) {
  return /^(?:margin(?:-|$)|padding(?:-|$)|gap$|row-gap$|column-gap$)/.test(prop);
}

function selectorState(selector) {
  const states = [];
  if (/:hover\b/.test(selector)) states.push('hover');
  if (/:active\b/.test(selector)) states.push('active');
  if (/:focus(?:-visible|-within)?\b/.test(selector)) states.push('focus');
  if (/:disabled\b|\[disabled\]|\[aria-disabled=['\"]?true/.test(selector)) states.push('disabled');
  if (/\[aria-busy=['\"]?true|\.is-loading|--loading\b/.test(selector)) states.push('loading');
  if (/\.pool-(?:empty|empty-state)|--empty\b/.test(selector)) states.push('empty');
  if (/\.pool-(?:error|field__error|banner--danger)|--error\b|\[data-error=['\"]?true/.test(selector)) states.push('error');
  return states;
}

function readArg(name) {
  const index = process.argv.indexOf(name);
  return index === -1 ? null : process.argv[index + 1] || null;
}

const sourceFiles = walk(srcRoot, SOURCE_EXTENSIONS).filter((file) => !file.endsWith('.d.ts'));
const cssFiles = walk(stylesRoot, CSS_EXTENSIONS);
const metrics = {
  generatedAt: new Date().toISOString(),
  scope: {
    sourceFiles: sourceFiles.length,
    cssFiles: cssFiles.length,
    excludes: ['node_modules', 'dist', 'tests'],
  },
  routes: [],
  tokens: { space: [], text: [], type: [], motion: [], allPool: 0 },
  typography: {
    cssFontSize: { total: 0, hardcodedPx: 0, values: [] },
    jsxInlineFontSize: { total: 0, values: [] },
    jsxFontSizeProperties: { total: 0, values: [] },
    lineHeight: [],
    fontWeight: [],
    letterSpacing: [],
    mixedScriptSpacingDecls: [],
  },
  layout: {
    spacingPixelOccurrences: 0,
    multipleOf8: 0,
    multipleOf4: 0,
    rhythmPixelOccurrences: 0,
    rhythmMultipleOf8: 0,
    rhythmMultipleOf4: 0,
    rhythmOffFour: [],
    offFour: [],
    breakpoints: [],
  },
  motion: { literalMsDeclarations: 0, values: [], animations: [], transitions: [] },
  numeric: { tabularDeclarations: 0, declarations: [], formatterSinks: [], formatterByFile: [] },
  i18n: {
    literalSites: 0, hanCharacters: 0, nonLocaleLiteralSites: 0, nonLocaleHanCharacters: 0,
    kinds: { stringLiteral: { sites: 0, hanCharacters: 0 }, templateElement: { sites: 0, hanCharacters: 0 }, jsxText: { sites: 0, hanCharacters: 0 } },
    nonLocaleKinds: { stringLiteral: { sites: 0, hanCharacters: 0 }, templateElement: { sites: 0, hanCharacters: 0 }, jsxText: { sites: 0, hanCharacters: 0 } },
    topFiles: [], byFile: [],
  },
  performance: {
    layoutReadSites: [],
    styleWriteSites: [],
    requestAnimationFrameSites: [],
    dynamicImports: [],
  },
  interaction: {
    cssStateSelectors: {}, asyncCallSites: [], nativeControls: [],
    asyncProtocols: { instantMutationCalls: [], tanstackMutationCalls: [], explicitButtonLoading: [] },
  },
};

const cssFontSizes = new Map();
const jsxFontSizes = new Map();
const jsxFontSizeProperties = new Map();
const lineHeights = new Map();
const fontWeights = new Map();
const letterSpacings = new Map();
const motionValues = new Map();
const tabularDecls = [];
const formatterSinks = [];
const hanByFile = new Map();
const hanCharactersByFile = new Map();
const layoutReadSites = [];
const styleWriteSites = [];
const rafSites = [];
const dynamicImports = [];
const asyncCallSites = [];
const nativeControls = [];
const instantMutationCalls = [];
const tanstackMutationCalls = [];
const explicitButtonLoading = [];
const stateSelectors = new Map([
  ['hover', []], ['active', []], ['focus', []], ['disabled', []],
  ['loading', []], ['empty', []], ['error', []],
]);
const breakpoints = [];
const animations = [];
const transitions = [];
const offFour = new Map();
const rhythmOffFour = new Map();

for (const file of sourceFiles) {
  const ast = parseSource(file);
  const sourceFile = rel(file);
  const inLocale = sourceFile.includes('/lib/locales/');
  traverse(ast, {
    StringLiteral(pathRef) {
      const value = pathRef.node.value;
      if (!HAN.test(value)) return;
      const characters = (value.match(HAN_GLOBAL) || []).length;
      metrics.i18n.literalSites += 1;
      metrics.i18n.hanCharacters += characters;
      metrics.i18n.kinds.stringLiteral.sites += 1;
      metrics.i18n.kinds.stringLiteral.hanCharacters += characters;
      if (!inLocale) {
        metrics.i18n.nonLocaleLiteralSites += 1;
        metrics.i18n.nonLocaleHanCharacters += characters;
        metrics.i18n.nonLocaleKinds.stringLiteral.sites += 1;
        metrics.i18n.nonLocaleKinds.stringLiteral.hanCharacters += characters;
        addFileCount(hanByFile, sourceFile);
        addFileCount(hanCharactersByFile, sourceFile, characters);
      }
    },
    TemplateElement(pathRef) {
      const value = stringValue(pathRef.node) || '';
      if (!HAN.test(value)) return;
      const characters = (value.match(HAN_GLOBAL) || []).length;
      metrics.i18n.literalSites += 1;
      metrics.i18n.hanCharacters += characters;
      metrics.i18n.kinds.templateElement.sites += 1;
      metrics.i18n.kinds.templateElement.hanCharacters += characters;
      if (!inLocale) {
        metrics.i18n.nonLocaleLiteralSites += 1;
        metrics.i18n.nonLocaleHanCharacters += characters;
        metrics.i18n.nonLocaleKinds.templateElement.sites += 1;
        metrics.i18n.nonLocaleKinds.templateElement.hanCharacters += characters;
        addFileCount(hanByFile, sourceFile);
        addFileCount(hanCharactersByFile, sourceFile, characters);
      }
    },
    JSXText(pathRef) {
      const value = pathRef.node.value;
      if (!HAN.test(value)) return;
      const characters = (value.match(HAN_GLOBAL) || []).length;
      metrics.i18n.literalSites += 1;
      metrics.i18n.hanCharacters += characters;
      metrics.i18n.kinds.jsxText.sites += 1;
      metrics.i18n.kinds.jsxText.hanCharacters += characters;
      if (!inLocale) {
        metrics.i18n.nonLocaleLiteralSites += 1;
        metrics.i18n.nonLocaleHanCharacters += characters;
        metrics.i18n.nonLocaleKinds.jsxText.sites += 1;
        metrics.i18n.nonLocaleKinds.jsxText.hanCharacters += characters;
        addFileCount(hanByFile, sourceFile);
        addFileCount(hanCharactersByFile, sourceFile, characters);
      }
    },
    JSXAttribute(pathRef) {
      const node = pathRef.node;
      const name = node.name.type === 'JSXIdentifier' ? node.name.name : '';
      if (name !== 'style' || node.value?.type !== 'JSXExpressionContainer') return;
      const object = node.value.expression;
      const property = objectProperty(object, 'fontSize');
      if (!property) return;
      const value = literalNumber(property.value) || '<dynamic>';
      addLocation(jsxFontSizes, value, loc(file, property));
    },
    ObjectProperty(pathRef) {
      const key = pathRef.node.key.type === 'Identifier' ? pathRef.node.key.name : stringValue(pathRef.node.key);
      if (key !== 'fontSize') return;
      const value = literalNumber(pathRef.node.value) || '<dynamic>';
      addLocation(jsxFontSizeProperties, value, loc(file, pathRef.node));
    },
    JSXOpeningElement(pathRef) {
      const name = pathRef.node.name.type === 'JSXIdentifier' ? pathRef.node.name.name : '<member>';
      if (['button', 'input', 'select', 'textarea'].includes(name)) nativeControls.push({ file: sourceFile, line: pathRef.node.loc.start.line, name });
      if (name === 'Button' && pathRef.node.attributes.some((attribute) => attribute.type === 'JSXAttribute'
        && attribute.name.type === 'JSXIdentifier' && attribute.name.name === 'loading')) {
        explicitButtonLoading.push({ file: sourceFile, line: pathRef.node.loc.start.line });
      }
    },
    CallExpression(pathRef) {
      const node = pathRef.node;
      if (node.callee.type === 'Import') {
        const value = stringValue(node.arguments[0]);
        dynamicImports.push({ file: sourceFile, line: node.loc.start.line, target: value || '<dynamic>' });
      }
      const callee = node.callee;
      if (callee.type === 'MemberExpression' && !callee.computed && callee.property.type === 'Identifier') {
        const member = callee.property.name;
        if (['getBoundingClientRect', 'getClientRects', 'getComputedStyle'].includes(member)) {
          layoutReadSites.push({ file: sourceFile, line: node.loc.start.line, api: member });
        }
        if (['requestAnimationFrame'].includes(member)) rafSites.push({ file: sourceFile, line: node.loc.start.line, api: member });
      }
      if (callee.type === 'Identifier' && ['requestAnimationFrame'].includes(callee.name)) rafSites.push({ file: sourceFile, line: node.loc.start.line, api: callee.name });
      if (callee.type === 'Identifier' && callee.name === 'useInstantMutation') instantMutationCalls.push({ file: sourceFile, line: node.loc.start.line });
      if (callee.type === 'Identifier' && callee.name === 'useMutation') tanstackMutationCalls.push({ file: sourceFile, line: node.loc.start.line });
      if (callee.type === 'Identifier' && /^(?:get|post|put|patch|del|mutate|mutateAsync|reload|refetch)$/.test(callee.name)) {
        asyncCallSites.push({ file: sourceFile, line: node.loc.start.line, api: callee.name });
      }
      if (callee.type === 'MemberExpression' && !callee.computed && callee.property.type === 'Identifier' && /^(?:toLocaleString|toFixed)$/.test(callee.property.name)) {
        formatterSinks.push({ file: sourceFile, line: node.loc.start.line, api: callee.property.name });
      }
    },
    ImportExpression(pathRef) {
      dynamicImports.push({ file: sourceFile, line: pathRef.node.loc.start.line, target: stringValue(pathRef.node.source) || '<dynamic>' });
    },
    MemberExpression(pathRef) {
      const node = pathRef.node;
      if (node.computed || node.property.type !== 'Identifier') return;
      if (['offsetWidth', 'offsetHeight', 'clientWidth', 'clientHeight', 'scrollWidth', 'scrollHeight', 'offsetTop', 'offsetLeft'].includes(node.property.name)) {
        layoutReadSites.push({ file: sourceFile, line: node.loc.start.line, api: node.property.name });
      }
      if (node.object.type === 'MemberExpression' && !node.object.computed && node.object.property.type === 'Identifier'
        && node.object.property.name === 'style') {
        styleWriteSites.push({ file: sourceFile, line: node.loc.start.line, property: node.property.name });
      }
    },
    ObjectExpression(pathRef) {
      if (!isRouteObject(pathRef.node)) return;
      const get = (name) => routeValue(propertyByName(pathRef.node, name));
      const lazy = propertyByName(pathRef.node, 'lazyLoader')?.value;
      let component = '<dynamic>';
      if (lazy?.type === 'ArrowFunctionExpression') {
        component = importTarget(lazy.body) || component;
      }
      metrics.routes.push({
        path: get('path'), role: get('role'), navGroup: get('navGroup'), prefetch: get('prefetch') || 'default', component,
        file: sourceFile, line: pathRef.node.loc.start.line,
      });
    },
  });
}

for (const file of cssFiles) {
  const css = fs.readFileSync(file, 'utf8');
  const sourceFile = rel(file);
  const rootAst = postcss.parse(css, { from: file });
  rootAst.walkAtRules('media', (atRule) => {
    const value = atRule.params;
    if (/max-width|min-width/.test(value)) breakpoints.push({ file: sourceFile, line: atRule.source.start.line, query: value });
  });
  rootAst.walkRules((rule) => {
    const states = selectorState(rule.selector);
    for (const state of states) {
      const entries = stateSelectors.get(state);
      if (entries.length < 80) entries.push({ file: sourceFile, line: rule.source.start.line, selector: rule.selector });
    }
  });
  rootAst.walkDecls((decl) => {
    const where = `${sourceFile}:${decl.source.start.line}`;
    const prop = decl.prop.toLowerCase();
    const value = decl.value.trim();
    if (prop.startsWith('--pool-')) metrics.tokens.allPool += 1;
    if (prop.startsWith('--pool-space-')) metrics.tokens.space.push({ name: prop, value, file: sourceFile, line: decl.source.start.line });
    if (prop === '--pool-text' || prop.startsWith('--pool-text-')) metrics.tokens.text.push({ name: prop, value, file: sourceFile, line: decl.source.start.line });
    if (prop.startsWith('--pool-type-')) metrics.tokens.type.push({ name: prop, value, file: sourceFile, line: decl.source.start.line });
    if (prop.startsWith('--pool-motion-')) metrics.tokens.motion.push({ name: prop, value, file: sourceFile, line: decl.source.start.line });

    if (prop === 'font-size') {
      metrics.typography.cssFontSize.total += 1;
      if (listPixelValues(value).length) metrics.typography.cssFontSize.hardcodedPx += 1;
      addLocation(cssFontSizes, value, where);
    }
    if (prop === 'line-height') addLocation(lineHeights, value, where);
    if (prop === 'font-weight') addLocation(fontWeights, value, where);
    if (prop === 'letter-spacing') addLocation(letterSpacings, value, where);
    if (prop === 'word-spacing' || prop === 'text-spacing' || prop === 'font-variant-east-asian') {
      metrics.typography.mixedScriptSpacingDecls.push({ file: sourceFile, line: decl.source.start.line, prop, value });
    }
    if (prop === 'font-variant-numeric' && value.includes('tabular-nums')) {
      tabularDecls.push({ file: sourceFile, line: decl.source.start.line, selector: decl.parent.selector, value });
    }
    if (propertyIsSpacing(prop)) {
      for (const pixel of listPixelValues(value)) {
        metrics.layout.spacingPixelOccurrences += 1;
        if (Number.isInteger(pixel) && pixel % 8 === 0) metrics.layout.multipleOf8 += 1;
        if (Number.isInteger(pixel) && pixel % 4 === 0) metrics.layout.multipleOf4 += 1;
        if (!Number.isInteger(pixel) || pixel % 4 !== 0) addLocation(offFour, `${pixel}px`, where);
      }
    }
    if (propertyIsRhythm(prop)) {
      for (const pixel of listPixelValues(value)) {
        metrics.layout.rhythmPixelOccurrences += 1;
        if (Number.isInteger(pixel) && pixel % 8 === 0) metrics.layout.rhythmMultipleOf8 += 1;
        if (Number.isInteger(pixel) && pixel % 4 === 0) metrics.layout.rhythmMultipleOf4 += 1;
        if (!Number.isInteger(pixel) || pixel % 4 !== 0) addLocation(rhythmOffFour, `${pixel}px`, where);
      }
    }
    if (/\b\d+(?:\.\d+)?ms\b/.test(value)) {
      metrics.motion.literalMsDeclarations += 1;
      for (const match of value.matchAll(/\b\d+(?:\.\d+)?ms\b/g)) addLocation(motionValues, match[0], where);
    }
    if (prop === 'animation' || prop === 'animation-name') animations.push({ file: sourceFile, line: decl.source.start.line, prop, value });
    if (prop === 'transition' || prop === 'transition-property') transitions.push({ file: sourceFile, line: decl.source.start.line, prop, value });
  });
}

metrics.routes.sort((a, b) => a.role.localeCompare(b.role) || a.path.localeCompare(b.path));
metrics.tokens.space.sort((a, b) => a.name.localeCompare(b.name));
metrics.tokens.text.sort((a, b) => a.name.localeCompare(b.name));
metrics.tokens.type.sort((a, b) => a.name.localeCompare(b.name));
metrics.tokens.motion.sort((a, b) => a.name.localeCompare(b.name));
metrics.typography.cssFontSize.values = compactEntries(cssFontSizes);
metrics.typography.jsxInlineFontSize.total = [...jsxFontSizes.values()].reduce((sum, entry) => sum + entry.count, 0);
metrics.typography.jsxInlineFontSize.values = compactEntries(jsxFontSizes);
metrics.typography.jsxFontSizeProperties.total = [...jsxFontSizeProperties.values()].reduce((sum, entry) => sum + entry.count, 0);
metrics.typography.jsxFontSizeProperties.values = compactEntries(jsxFontSizeProperties);
metrics.typography.lineHeight = compactEntries(lineHeights);
metrics.typography.fontWeight = compactEntries(fontWeights);
metrics.typography.letterSpacing = compactEntries(letterSpacings);
metrics.layout.offFour = compactEntries(offFour);
metrics.layout.rhythmOffFour = compactEntries(rhythmOffFour);
metrics.layout.breakpoints = breakpoints;
metrics.motion.values = compactEntries(motionValues);
metrics.motion.animations = animations;
metrics.motion.transitions = transitions;
metrics.numeric.tabularDeclarations = tabularDecls.length;
metrics.numeric.declarations = tabularDecls;
metrics.numeric.formatterSinks = formatterSinks;
metrics.numeric.formatterByFile = topFileCounts(new Map(formatterSinks.map((entry) => [entry.file, formatterSinks.filter((item) => item.file === entry.file).length])));
metrics.i18n.topFiles = topFileCounts(hanByFile);
metrics.i18n.byFile = [...hanByFile.entries()]
  .map(([file, literalSites]) => ({ file, literalSites, hanCharacters: hanCharactersByFile.get(file) || 0 }))
  .sort((a, b) => a.file.localeCompare(b.file));
metrics.performance.layoutReadSites = layoutReadSites;
metrics.performance.styleWriteSites = styleWriteSites;
metrics.performance.requestAnimationFrameSites = rafSites;
metrics.performance.dynamicImports = dynamicImports;
metrics.interaction.cssStateSelectors = Object.fromEntries([...stateSelectors.entries()].map(([state, entries]) => [state, { count: entries.length, examples: entries.slice(0, 20) }]));
metrics.interaction.asyncCallSites = asyncCallSites;
metrics.interaction.nativeControls = nativeControls;
metrics.interaction.asyncProtocols = { instantMutationCalls, tanstackMutationCalls, explicitButtonLoading };

const out = readArg('--out');
const payload = `${JSON.stringify(metrics, null, 2)}\n`;
if (out) {
  fs.mkdirSync(path.dirname(path.resolve(out)), { recursive: true });
  fs.writeFileSync(out, payload);
} else {
  process.stdout.write(payload);
}
console.error(`Aurora P0 static: ${metrics.routes.length} routes, ${metrics.scope.sourceFiles} source files, ${metrics.scope.cssFiles} CSS files; output ${out || 'stdout'}`);
