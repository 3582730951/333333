#!/usr/bin/env node
/**
 * Aurora P2a token migration map.
 *
 * Uses PostCSS for CSS and Babel ASTs for source, so every recommendation has a
 * stable source location. It reports candidates only: P2b owns the replacements.
 *
 * Usage:
 *   node scripts/audit/map-token-migration.mjs --out /tmp/aurora-p2-token-map.json --summary
 *   node scripts/audit/map-token-migration.mjs --format markdown
 */
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

import parser from '@babel/parser';
import traverseModule from '@babel/traverse';
import postcss from 'postcss';

const traverse = traverseModule.default;
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const srcRoot = path.join(root, 'src');
const stylesRoot = path.join(srcRoot, 'styles');
const tokensPath = path.join(stylesRoot, 'tokens.css');
const SOURCE_EXTENSIONS = new Set(['.js', '.jsx', '.ts', '.tsx']);
const CSS_EXTENSIONS = new Set(['.css']);
const PX_PATTERN = /(-?(?:\d*\.\d+|\d+))px\b/g;
const MS_PATTERN = /(-?(?:\d*\.\d+|\d+))ms\b/g;

const TYPE_RULES = [
  { value: 12, role: 'caption' },
  { value: 13, role: 'label' },
  { value: 14, role: 'body' },
  { value: 16, role: 'callout' },
  { value: 18, role: 'title-3' },
  { value: 24, role: 'title-2' },
  { value: 28, role: 'title-1' },
  { value: 48, role: 'display' },
].map(({ value, role }) => ({
  value,
  token: `--pool-type-${role}`,
  desktopToken: `--pool-type-${role}-desktop`,
  mobileToken: `--pool-type-${role}-mobile`,
}));

const SPACE_RULES = [
  [4, '--pool-space-1'],
  [8, '--pool-space-2'],
  [12, '--pool-space-3'],
  [16, '--pool-space-4'],
  [20, '--pool-space-5'],
  [24, '--pool-space-6'],
  [28, '--pool-space-7'],
  [32, '--pool-space-8'],
  [36, '--pool-space-9'],
  [40, '--pool-space-10'],
  [48, '--pool-space-12'],
  [64, '--pool-space-16'],
  [80, '--pool-space-20'],
  [96, '--pool-space-24'],
];

const MOTION_RULES = [
  [100, '--pool-motion-instant'],
  [150, '--pool-motion-fast'],
  [200, '--pool-motion-normal'],
  [300, '--pool-motion-slow'],
];

const BREAKPOINT_RULES = [
  [767, '--pool-breakpoint-page-compact'],
  [1100, '--pool-breakpoint-page-expanded'],
];

// A media query cannot read a custom property, so the contract is enforced on the literal
// operand. `max-width: 767px` and `min-width: 768px` are strict complements at integer viewport
// widths -- 767 and below hit the first, 768 and above the second, with neither a gap nor an
// overlap -- so the legal operand set is each contract value plus its +1 partner. Deriving it
// rather than hardcoding {767,768,1100,1101} means adding a breakpoint token carries its
// complement along automatically instead of minting a fresh exception.
const ALLOWED_BREAKPOINT_WIDTHS = new Set(BREAKPOINT_RULES.flatMap(([value]) => [value, value + 1]));

// Adjudicated out-of-contract breakpoints, from the 39-row decision table in
// .run/codex-tasks/aurora-P2b-2-D.out.md section 1.1. This table is the *judgement*; the JSON
// baseline beside it is *derived* from this table plus the current source, which is why the
// baseline is regenerated with --write-baseline rather than hand-edited.
//
// bucket 'approved' = D class B (component-scoped, keep) and class C (device tier, awaiting the
// user's signature). Both are decided: the audit stays silent on them.
// bucket 'pending' = D class A (should converge on 767/1100) and class E (dead code). These are
// to-do items, not exemptions. They are reported loudly on every run but do not fail the gate,
// because failing on them would block the very batches that remove them. When a batch lands,
// regenerate: a removed occurrence drops out of the baseline on its own.
//
// Keyed by `file|normalized params|width`, with one entry per occurrence in document order.
// Line numbers are deliberately absent: they drift on every edit to the file above them, and a
// key that drifts is a key that silently stops matching. `count` is the occurrence count this
// judgement was made against -- if it no longer matches, ordinals may have shifted and a
// mixed-bucket group must be re-adjudicated rather than guessed at.
const BREAKPOINT_DECISIONS = {
  'src/styles/atmosphere.css|(max-width:720px)|720': { count: 1, items: [
    { item: 1, klass: 'E', bucket: 'pending', note: 'dead: .pool-bento has zero JSX consumers, all 5 hits are inside atmosphere.css' },
  ] },
  'src/styles/components.css|(min-width:620px)|620': { count: 1, items: [
    { item: 2, klass: 'B', bucket: 'approved', note: 'tab strip inside OAuthLoginModal; modal width is not viewport width' },
  ] },
  'src/styles/components.css|(max-width:1180px)|1180': { count: 2, items: [
    { item: 3, klass: 'B', bucket: 'approved', note: '.pool-registration-start-form 4->2 col, intra-form wrap' },
    { item: 20, klass: 'B', bucket: 'approved', note: '.pool-cf-mail-layout, single page (CloudflareMailbox.tsx)' },
  ] },
  'src/styles/components.css|(max-width:900px)|900': { count: 1, items: [
    { item: 4, klass: 'B', bucket: 'approved', note: '.pool-sms-price-policy, content-driven per in-place comment' },
  ] },
  'src/styles/components.css|(max-width:560px)|560': { count: 1, items: [
    { item: 5, klass: 'B', bucket: 'approved', note: 'final collapse of the same sms component' },
  ] },
  'src/styles/components.css|(max-width:1000px)|1000': { count: 1, items: [
    { item: 6, klass: 'A', bucket: 'pending', note: 'converge to 1100: .pool-grid.cols-* is the only genuinely cross-route grid' },
  ] },
  'src/styles/components.css|(max-width:640px)|640': { count: 3, items: [
    { item: 7, klass: 'B', bucket: 'approved', note: '.pool-egress-* form grid, inside EgressProfileForm.jsx' },
    { item: 13, klass: 'A', bucket: 'pending', note: '4 declarations dead (shadowed by the pages.css 767 block); nth-child border resets stay' },
    { item: 16, klass: 'B', bucket: 'approved', note: '.pool-table--cards row heights; optional merge into 767' },
  ] },
  'src/styles/components.css|(max-width:1080px)|1080': { count: 1, items: [
    { item: 8, klass: 'B', bucket: 'approved', note: '.pool-email-metrics column count coupled to nth-child(3n+1) borders' },
  ] },
  'src/styles/components.css|(max-width:1024px)|1024': { count: 1, items: [
    { item: 9, klass: 'A', bucket: 'pending', note: 'login page is off the page grid; scope question needs the user before converging' },
  ] },
  'src/styles/components.css|(max-width:420px)|420': { count: 1, items: [
    { item: 10, klass: 'C', bucket: 'approved', note: 'hand-picked ultra-narrow tier; a sub-767 third tier needs the user signature' },
  ] },
  'src/styles/components.css|(min-width:768px) and (max-width:900px)|900': { count: 1, items: [
    { item: 12, klass: 'B', bucket: 'approved', note: 'upper bound of the AI settings two-col->one-col; the 768 operand is in contract' },
  ] },
  'src/styles/components.css|(max-width:480px)|480': { count: 1, items: [
    { item: 14, klass: 'B', bucket: 'approved', note: 'anti-truncation on .pool-settings-category__trigger, not a grid' },
  ] },
  'src/styles/components.css|(max-width:390px)|390': { count: 1, items: [
    { item: 15, klass: 'C', bucket: 'approved', note: '390 = iPhone 12-15 logical width, real tier; one selector inside is dead (see D 1.5)' },
  ] },
  'src/styles/components.css|(max-width:700px)|700': { count: 1, items: [
    { item: 21, klass: 'B', bucket: 'approved', note: 'Sub2APIHubPanel.jsx; cheapest B-class merge into 767 if ever wanted' },
  ] },
  'src/styles/dataviz.css|(max-width:1180px)|1180': { count: 1, items: [
    { item: 22, klass: 'B', bucket: 'approved', note: '.pool-ops-split inside a Dashboard panel' },
  ] },
  'src/styles/dataviz.css|(min-width:540px) and (max-width:767px)|540': { count: 1, items: [
    { item: 24, klass: 'B', bucket: 'approved', note: 'in-band lower bound for the exactly-five-block kpi strip; upper bound 767 is in contract' },
  ] },
  'src/styles/dataviz.css|(max-width:900px)|900': { count: 3, items: [
    { item: 25, klass: 'B', bucket: 'approved', note: '.pool-quota-overview -> 1 col; part of the 900 cluster awaiting signature' },
    { item: 26, klass: 'B', bucket: 'approved', note: '.pool-quota-credits__body -> 1 col; same cluster' },
    { item: 27, klass: 'B', bucket: 'approved', note: '.pool-mq-overview -> 1 col; same cluster' },
  ] },
  'src/styles/dataviz.css|(max-width:560px)|560': { count: 1, items: [
    { item: 28, klass: 'B', bucket: 'approved', note: '.pool-frontend-performance__controls, System.tsx control bar' },
  ] },
  'src/styles/dataviz.css|(max-width:460px)|460': { count: 1, items: [
    { item: 29, klass: 'B', bucket: 'approved', note: 'D calls this the model B-class breakpoint: in-place comment gives the exact measurement' },
  ] },
  // Was count 2 -- items #30 (dead, `padding: 12px`, shadowed by the later block) and #32 (live).
  // Batch 1 deleted #30; verified by content, not by position: the surviving block is the one
  // carrying `max(var(--pool-space-5), env(safe-area-inset-bottom))`, which is #32. Re-adjudicated
  // here because the ordinal guard refused to guess which verdict the survivor inherited.
  'src/styles/layout.css|(max-width:640px)|640': { count: 1, items: [
    { item: 32, klass: 'A', bucket: 'pending', note: 'converge to 767, but max(...,env(safe-area-inset-bottom)) must move verbatim (R2)' },
  ] },
  'src/styles/layout.css|(max-width:520px)|520': { count: 1, items: [
    { item: 31, klass: 'A', bucket: 'pending', note: 'half dead: width:100% already set by the 767 block; only max-width:100% on children is live' },
  ] },
  'src/styles/pages.css|(max-width:1120px)|1120': { count: 1, items: [
    { item: 33, klass: 'A', bucket: 'pending', note: 'lowest-risk A convergence, delta 20px to 1100' },
  ] },
  'src/styles/pages.css|(min-width:768px) and (max-width:1360px)|1360': { count: 1, items: [
    { item: 35, klass: 'C', bucket: 'approved', note: '1360 ~ 1366 laptop tier; every contract-legal rewrite changes behaviour' },
  ] },
  'src/styles/pages.css|(max-width:900px)|900': { count: 1, items: [
    { item: 36, klass: 'A', bucket: 'pending', note: 'split: .pool-grid halves are dead (shadowed by components.css 1000px), nav item is live -- do not delete the block' },
  ] },
  'src/styles/pages.css|(max-width:420px)|420': { count: 1, items: [
    { item: 37, klass: 'C', bucket: 'approved', note: 'pairs with item 10 as the narrow-phone tier; also carries an out-of-contract 12px gutter' },
  ] },
  'src/styles/portal.css|(max-width:1080px)|1080': { count: 1, items: [
    { item: 38, klass: 'B', bucket: 'approved', note: '.pool-portal-bento 2 col; the 1080 match with item 8 is coincidence' },
  ] },
  // D7 normalized the portal bento boundary to 520px. Keep the approved device-tier
  // exception keyed to the post-decision source; leaving the old 519px key here makes
  // the contract gate treat the intentional one-pixel correction as an unreviewed rule.
  'src/styles/portal.css|(max-width:520px)|520': { count: 1, items: [
    { item: 39, klass: 'C', bucket: 'approved', note: 'D7 normalized the overlapping 519px boundary to 520px; portal bento device tier' },
  ] },
};

const EASING_RULES = new Map([
  ['linear', '--pool-ease-linear'],
  ['cubic-bezier(.2,.8,.2,1)', '--pool-ease-standard'],
  ['cubic-bezier(.2,0,0,1)', '--pool-ease-emphasized'],
  ['cubic-bezier(0,0,.2,1)', '--pool-ease-enter'],
  ['cubic-bezier(.4,0,1,1)', '--pool-ease-exit'],
]);

const typeByValue = new Map(TYPE_RULES.map((rule) => [rule.value, rule]));
const spaceByValue = new Map(SPACE_RULES);
const motionByValue = new Map(MOTION_RULES);
const breakpointByValue = new Map(BREAKPOINT_RULES);

function walk(dir, extensions) {
  const files = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const file = path.join(dir, entry.name);
    if (entry.isDirectory()) files.push(...walk(file, extensions));
    else if (extensions.has(path.extname(entry.name))) files.push(file);
  }
  return files.sort();
}

function rel(file) {
  return path.relative(root, file).replaceAll(path.sep, '/');
}

function readArg(name) {
  const index = process.argv.indexOf(name);
  return index === -1 ? null : process.argv[index + 1] || null;
}

function hasFlag(name) {
  return process.argv.includes(name);
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

function objectPropertyName(node) {
  if (node.type !== 'ObjectProperty') return null;
  if (node.key.type === 'Identifier') return node.key.name;
  if (node.key.type === 'StringLiteral') return node.key.value;
  return null;
}

function expressionValue(node) {
  if (!node) return null;
  if (node.type === 'NumericLiteral') return { raw: String(node.value), numeric: node.value };
  if (node.type === 'StringLiteral') return { raw: node.value, numeric: null };
  if (node.type === 'TemplateLiteral' && node.expressions.length === 0) {
    return { raw: node.quasis.map((quasi) => quasi.value.cooked ?? quasi.value.raw).join(''), numeric: null };
  }
  return null;
}

function pxMatches(value) {
  return [...String(value).matchAll(PX_PATTERN)].map((match, index) => ({
    index,
    position: match.index,
    raw: match[0],
    value: Number(match[1]),
  }));
}

function msMatches(value) {
  return [...String(value).matchAll(MS_PATTERN)].map((match, index) => ({
    index,
    position: match.index,
    raw: match[0],
    value: Number(match[1]),
  }));
}

function isRhythmProperty(property) {
  return /^(?:margin(?:-|$)|padding(?:-|$)|gap$|row-gap$|column-gap$)/.test(property);
}

function isMotionProperty(property) {
  return /^(?:animation|animation-duration|animation-delay|transition|transition-duration|transition-delay)$/.test(property);
}

function normalizeCurve(value) {
  return value.replaceAll(/\s+/g, '').replaceAll('0.', '.');
}

function tokenDeclarations() {
  const declarations = new Map();
  const css = fs.readFileSync(tokensPath, 'utf8');
  postcss.parse(css, { from: tokensPath }).walkDecls((decl) => declarations.set(decl.prop, decl.value.trim()));
  return declarations;
}

function verifyTokenContract() {
  const tokens = tokenDeclarations();
  const expected = [
    ...TYPE_RULES.map((rule) => [`--pool-font-size-${rule.value}`, `${rule.value}px`]),
    ...SPACE_RULES.map(([value, token]) => [token, `${value}px`]),
    ...MOTION_RULES.map(([value, token]) => [token, `${value}ms`]),
    ...BREAKPOINT_RULES.map(([value, token]) => [token, `${value}px`]),
  ];
  const failures = expected.filter(([name, value]) => tokens.get(name) !== value)
    .map(([name, value]) => `${name} must equal ${value}; found ${tokens.get(name) || '<missing>'}`);
  if (failures.length) throw new Error(`Token contract is incomplete:\n${failures.join('\n')}`);
}

function recordBase(domain, file, line, property, declarationValue, occurrence, value) {
  return {
    domain,
    source: `${rel(file)}:${line}`,
    file: rel(file),
    line,
    property,
    declarationValue,
    occurrence,
    value,
  };
}

function classifyType(value, record) {
  const rule = typeByValue.get(value);
  if (!rule) {
    return {
      ...record,
      status: 'exception',
      reason: 'outside-type-scale',
      suggestedReview: TYPE_RULES.map((candidate) => candidate.value),
    };
  }
  return {
    ...record,
    status: 'mapped',
    token: rule.token,
    desktopToken: rule.desktopToken,
    mobileToken: rule.mobileToken,
    replacement: `var(${rule.token})`,
  };
}

function classifySpacing(value, record, canReplace) {
  if (value === 0) {
    return {
      ...record,
      status: 'normalize',
      reason: 'css-zero',
      replacement: '0',
    };
  }
  if (!canReplace) {
    return {
      ...record,
      status: 'exception',
      reason: 'spacing-expression-requires-manual-review',
    };
  }
  const token = spaceByValue.get(Math.abs(value));
  if (token) {
    return {
      ...record,
      status: 'mapped',
      token,
      replacement: value < 0 ? `calc(var(${token}) * -1)` : `var(${token})`,
    };
  }
  if (Math.abs(value) <= 3) {
    return {
      ...record,
      status: 'exception',
      reason: 'optical-adjustment-must-be-registered',
    };
  }
  return {
    ...record,
    status: 'exception',
    reason: 'outside-spacing-scale',
    suggestedReview: SPACE_RULES.map(([candidate]) => candidate),
  };
}

function classifyMotion(value, record, canReplace) {
  if (value === 0) return { ...record, status: 'normalize', reason: 'css-zero', replacement: '0ms' };
  if (!canReplace) return { ...record, status: 'exception', reason: 'motion-expression-requires-manual-review' };
  const token = motionByValue.get(value);
  if (token) return { ...record, status: 'mapped', token, replacement: `var(${token})` };
  return {
    ...record,
    status: 'exception',
    reason: 'outside-motion-duration-scale',
    suggestedReview: MOTION_RULES.map(([candidate]) => candidate),
  };
}

function staticFontPixels(value, numeric = null) {
  if (typeof numeric === 'number') return numeric;
  const trimmed = String(value).trim();
  const px = /^(-?(?:\d*\.\d+|\d+))px$/.exec(trimmed);
  if (px) return Number(px[1]);
  const unitless = /^-?(?:\d*\.\d+|\d+)$/.exec(trimmed);
  return unitless ? Number(trimmed) : null;
}

function variableTokenAt(value, position, prefix) {
  const variables = /var\(\s*(--[\w-]+)[^)]*\)/g;
  for (const match of value.matchAll(variables)) {
    const start = match.index;
    const end = start + match[0].length;
    if (position >= start && position < end && match[1].startsWith(prefix)) return match[1];
  }
  return null;
}

function tokenOnlyValue(value, prefix) {
  const match = new RegExp(`^var\\(\\s*(${prefix}[\\w-]+)(?:\\s*,[^)]*)?\\s*\\)$`).exec(value);
  return match?.[1] || null;
}

function withoutVariables(value) {
  return value.replaceAll(/var\([^)]*\)/g, 'var-token');
}

function isSimpleRhythmValue(value) {
  return !/[(),/]/.test(withoutVariables(value));
}

function isSimpleMotionValue(value) {
  return !/[()]/.test(withoutVariables(value));
}

function extractCurves(value) {
  const source = withoutVariables(value);
  const curves = [];
  const cubic = /cubic-bezier\([^)]*\)/g;
  for (const match of source.matchAll(cubic)) curves.push(match[0]);
  const keywords = /\b(?:linear|ease|ease-in|ease-out|ease-in-out|step-start|step-end)\b/g;
  for (const match of source.matchAll(keywords)) curves.push(match[0]);
  return curves;
}

function classifyCurve(value, record) {
  const token = EASING_RULES.get(normalizeCurve(value));
  if (token) return { ...record, status: 'mapped', token, replacement: `var(${token})` };
  return { ...record, status: 'exception', reason: 'outside-easing-library' };
}

function countByStatus(records) {
  return records.reduce((result, record) => {
    result.total += 1;
    result[record.status] = (result[record.status] || 0) + 1;
    return result;
  }, { total: 0, mapped: 0, alreadyTokenized: 0, normalize: 0, exception: 0 });
}

function sorted(records) {
  return records.sort((a, b) => a.file.localeCompare(b.file)
    || a.line - b.line
    || a.property.localeCompare(b.property)
    || a.occurrence - b.occurrence);
}

function markdown(report) {
  const lines = [
    '# Aurora P2 token migration map',
    '',
    `Generated: ${report.generatedAt}`,
    '',
    '| Domain | Total | Mapped | Already tokenized | Normalize | Exceptions |',
    '|---|---:|---:|---:|---:|---:|',
  ];
  for (const [domain, summary] of Object.entries(report.summary.byDomain)) {
    lines.push(`| ${domain} | ${summary.total} | ${summary.mapped} | ${summary.alreadyTokenized} | ${summary.normalize} | ${summary.exception} |`);
  }
  lines.push('', '## Exceptions', '');
  for (const entry of report.exceptions) {
    lines.push(`- ${entry.source} ${entry.property} ${entry.value}: ${entry.reason}`);
  }
  lines.push('');
  return lines.join('\n');
}

verifyTokenContract();

// Denominators for the breakpoint contract assertion. A zero here must fail rather than pass:
// "no breakpoint violates the contract" and "no breakpoint was ever examined" produce the same
// clean output otherwise.
const breakpointScan = { mediaBlocks: 0, widthMentions: 0, widthOperands: 0, cssFilesWalked: 0, unparsed: [] };

const cssFontSize = [];
const jsxFontSize = [];
const cssSpacing = [];
const cssBreakpoints = [];
const cssMotionDuration = [];
const cssMotionEasing = [];

for (const file of walk(srcRoot, SOURCE_EXTENSIONS).filter((candidate) => !candidate.endsWith('.d.ts'))) {
  const ast = parseSource(file);
  traverse(ast, {
    ObjectProperty(pathRef) {
      const property = pathRef.node;
      if (objectPropertyName(property) !== 'fontSize') return;
      const value = expressionValue(property.value);
      const record = recordBase(
        'jsx-font-size',
        file,
        property.loc.start.line,
        'fontSize',
        value?.raw || '<dynamic>',
        0,
        value?.raw || '<dynamic>',
      );
      const token = value ? tokenOnlyValue(value.raw.trim(), '--pool-type-') || tokenOnlyValue(value.raw.trim(), '--pool-font-size-') : null;
      if (token) {
        jsxFontSize.push({ ...record, status: 'alreadyTokenized', token });
        return;
      }
      const pixels = value ? staticFontPixels(value.raw, value.numeric) : null;
      if (pixels === null) {
        jsxFontSize.push({ ...record, status: 'exception', reason: 'dynamic-or-expression-font-size' });
        return;
      }
      jsxFontSize.push(classifyType(pixels, record));
    },
    JSXAttribute(pathRef) {
      const attribute = pathRef.node;
      if (attribute.name.type !== 'JSXIdentifier' || attribute.name.name !== 'fontSize') return;
      const value = attribute.value?.type === 'JSXExpressionContainer'
        ? expressionValue(attribute.value.expression)
        : attribute.value?.type === 'StringLiteral'
          ? { raw: attribute.value.value, numeric: null }
          : null;
      const record = recordBase(
        'jsx-font-size',
        file,
        attribute.loc.start.line,
        'fontSize',
        value?.raw || '<dynamic>',
        0,
        value?.raw || '<dynamic>',
      );
      const token = value ? tokenOnlyValue(value.raw.trim(), '--pool-type-') || tokenOnlyValue(value.raw.trim(), '--pool-font-size-') : null;
      if (token) {
        jsxFontSize.push({ ...record, status: 'alreadyTokenized', token });
        return;
      }
      const pixels = value ? staticFontPixels(value.raw, value.numeric) : null;
      if (pixels === null) {
        jsxFontSize.push({ ...record, status: 'exception', reason: 'dynamic-or-expression-font-size' });
        return;
      }
      jsxFontSize.push(classifyType(pixels, record));
    },
  });
}

for (const file of walk(stylesRoot, CSS_EXTENSIONS).filter((candidate) => candidate !== tokensPath)) {
  const css = fs.readFileSync(file, 'utf8');
  const ast = postcss.parse(css, { from: file });
  breakpointScan.cssFilesWalked += 1;
  ast.walkAtRules('media', (atRule) => {
    breakpointScan.mediaBlocks += 1;
    // The extractor below only reaches `(min|max)-width: <n>px`. Media Queries Level 4 range
    // syntax (`(400px <= width <= 700px)`, `(width < 768px)`) and em/rem operands are width
    // conditions it cannot see, and an unseen operand is not reported as unchecked -- it simply
    // never becomes a record, which reads exactly like a file with no breakpoints. Count the
    // width mentions independently and fail on any the extractor could not account for, so the
    // contract assertion can never be quietly bypassed by rewriting a query in newer syntax.
    const widthMentions = [...atRule.params.matchAll(/\bwidth\b/g)].length;
    const parsedHere = [...atRule.params.matchAll(/(?:min|max)-width\s*:\s*(-?(?:\d*\.\d+|\d+))px\b/g)].length;
    breakpointScan.widthMentions += widthMentions;
    breakpointScan.widthOperands += parsedHere;
    if (widthMentions !== parsedHere) {
      breakpointScan.unparsed.push({
        source: `${rel(file)}:${atRule.source.start.line}`,
        params: atRule.params.replaceAll(/\s+/g, ' ').trim(),
        widthMentions,
        parsed: parsedHere,
      });
    }
    for (const match of atRule.params.matchAll(/(?:min|max)-width\s*:\s*(-?(?:\d*\.\d+|\d+))px\b/g)) {
      const value = Number(match[1]);
      const record = recordBase(
        'css-breakpoint',
        file,
        atRule.source.start.line,
        '@media',
        atRule.params,
        cssBreakpoints.length,
        `${value}px`,
      );
      const token = breakpointByValue.get(value);
      cssBreakpoints.push(token
        ? { ...record, status: 'mapped', token, replacement: `${value}px`, reason: 'media-queries-cannot-consume-custom-properties' }
        : { ...record, status: 'exception', reason: 'outside-page-breakpoint-contract', suggestedReview: BREAKPOINT_RULES.map(([candidate]) => candidate) });
    }
  });
  ast.walkDecls((decl) => {
    const property = decl.prop.toLowerCase();
    const value = decl.value.trim();
    const line = decl.source.start.line;
    if (property === 'font-size') {
      const matches = pxMatches(value);
      if (matches.length) {
        const exact = /^-?(?:\d*\.\d+|\d+)px$/.exec(value);
        if (exact) {
          cssFontSize.push(classifyType(Number(value.slice(0, -2)), recordBase(
            'css-font-size', file, line, property, value, 0, value,
          )));
        } else {
          for (const match of matches) {
            const record = recordBase('css-font-size', file, line, property, value, match.index, match.raw);
            const token = variableTokenAt(value, match.position, '--pool-type-')
              || variableTokenAt(value, match.position, '--pool-font-size-');
            cssFontSize.push(token
              ? { ...record, status: 'alreadyTokenized', token }
              : { ...record, status: 'exception', reason: 'font-size-expression-requires-manual-review' });
          }
        }
      }
    }
    if (isRhythmProperty(property)) {
      const matches = pxMatches(value);
      const canReplace = isSimpleRhythmValue(value);
      for (const match of matches) {
        const record = recordBase('css-spacing', file, line, property, value, match.index, match.raw);
        const token = variableTokenAt(value, match.position, '--pool-space-');
        cssSpacing.push(token
          ? { ...record, status: 'alreadyTokenized', token }
          : classifySpacing(match.value, record, canReplace));
      }
    }
    if (isMotionProperty(property)) {
      const canReplace = isSimpleMotionValue(value);
      for (const match of msMatches(value)) {
        const record = recordBase('css-motion-duration', file, line, property, value, match.index, match.raw);
        const token = variableTokenAt(value, match.position, '--pool-motion-');
        cssMotionDuration.push(token
          ? { ...record, status: 'alreadyTokenized', token }
          : classifyMotion(match.value, record, canReplace));
      }
      for (const [index, curve] of extractCurves(value).entries()) {
        cssMotionEasing.push(classifyCurve(curve, recordBase(
          'css-motion-easing', file, line, property, value, index, curve,
        )));
      }
    }
  });
}

const mappings = {
  cssFontSize: sorted(cssFontSize),
  jsxFontSize: sorted(jsxFontSize),
  cssSpacing: sorted(cssSpacing),
  cssBreakpoints: sorted(cssBreakpoints),
  cssMotionDuration: sorted(cssMotionDuration),
  cssMotionEasing: sorted(cssMotionEasing),
};
const allRecords = Object.values(mappings).flat();
const summaryByDomain = Object.fromEntries(Object.entries(mappings).map(([domain, records]) => [domain, countByStatus(records)]));
const report = {
  schemaVersion: 1,
  generatedAt: new Date().toISOString(),
  scope: {
    root: rel(root),
    sourceFiles: walk(srcRoot, SOURCE_EXTENSIONS).filter((file) => !file.endsWith('.d.ts')).length,
    cssFiles: walk(stylesRoot, CSS_EXTENSIONS).filter((file) => file !== tokensPath).length,
    excluded: ['src/styles/tokens.css', 'node_modules', 'dist', 'tests'],
  },
  contracts: {
    type: TYPE_RULES,
    spacing: SPACE_RULES.map(([value, token]) => ({ value, token })),
    motion: MOTION_RULES.map(([value, token]) => ({ value, token })),
    breakpoints: BREAKPOINT_RULES.map(([value, token]) => ({ value, token })),
  },
  summary: {
    total: countByStatus(allRecords),
    byDomain: summaryByDomain,
  },
  mappings,
  exceptions: allRecords.filter((record) => record.status === 'exception'),
};

const format = readArg('--format') || 'json';
if (!['json', 'markdown'].includes(format)) throw new Error(`Unknown --format ${format}; use json or markdown.`);
const payload = format === 'markdown' ? markdown(report) : `${JSON.stringify(report, null, 2)}\n`;
const out = readArg('--out');
if (out) {
  const absolute = path.resolve(out);
  fs.mkdirSync(path.dirname(absolute), { recursive: true });
  fs.writeFileSync(absolute, payload);
}

if (hasFlag('--summary')) {
  process.stdout.write(`${JSON.stringify(report.summary, null, 2)}\n`);
} else if (!out && !hasFlag('--write-baseline') && !hasFlag('--quiet')) {
  // --write-baseline's own output is the diff-relevant part; 130KB of migration map on top of it
  // buries the one line saying what was written. --quiet exists for the same reason on the gate
  // path: run bare inside `npm run check`, this dump was 133KB of a 136KB log, and the three lines
  // carrying the contract verdict were unreadable in it.
  process.stdout.write(payload);
}

process.stderr.write(
  `Aurora P2 token map: ${report.summary.total.total} records; ${report.summary.total.mapped} mapped, `
  + `${report.summary.total.alreadyTokenized} already tokenized, ${report.summary.total.normalize} normalizable, `
  + `${report.summary.total.exception} exceptions; output ${out || 'stdout'}.\n`,
);

// ---------------------------------------------------------------------------
// Breakpoint contract assertion
// ---------------------------------------------------------------------------
// The page breakpoint contract admits four literal operands: 767/1100 and their +1 complements.
// Every other width in src/styles is a deviation. 33 of them exist today and are adjudicated in
// BREAKPOINT_DECISIONS above; this section fails the run on a *new* one, so the count can only go
// down. It deliberately does not touch record.status -- the migration map's own classification and
// its 187/129 totals stay exactly as they were.

const baselinePath = path.join(root, 'scripts', 'breakpoint-contract-baseline.json');

function normalizeParams(params) {
  return params.replaceAll(/\s+/g, ' ').replaceAll(/\s*:\s*/g, ':').trim();
}

function groupKeyOf(record, width) {
  return `${record.file}|${normalizeParams(record.declarationValue)}|${width}`;
}

// Occurrences, grouped, in document order. The group key carries no line number on purpose: a
// line number drifts whenever anything above it changes, and a baseline keyed on drifting data
// stops matching without saying so -- it would report every entry as both "gone" and "new".
function collectOutOfContract() {
  const groups = new Map();
  for (const record of mappings.cssBreakpoints) {
    const width = Number(String(record.value).replace('px', ''));
    if (!Number.isFinite(width)) throw new Error(`unparsable breakpoint width ${record.value} at ${record.source}`);
    if (ALLOWED_BREAKPOINT_WIDTHS.has(width)) continue;
    const key = groupKeyOf(record, width);
    if (!groups.has(key)) groups.set(key, { key, file: record.file, params: normalizeParams(record.declarationValue), width, lines: [] });
    groups.get(key).lines.push(record.line);
  }
  for (const group of groups.values()) group.lines.sort((a, b) => a - b);
  return groups;
}

// Attach D's adjudication to each occurrence. Ordinals are positions in document order within a
// group, which is stable under edits elsewhere in the file but *not* under deletion of a member of
// the group itself. So the judgement records the count it was made against: when the count no
// longer matches and the group's members do not all share one bucket, the ordinals can no longer
// be trusted to mean what they meant, and the group is flagged for re-adjudication instead of
// being silently mis-bucketed. A group whose members all agree is safe either way.
function adjudicate(group) {
  const decision = BREAKPOINT_DECISIONS[group.key];
  if (!decision) {
    return group.lines.map((line) => ({ line, bucket: 'unclassified', klass: null, item: null, note: 'no adjudication on record' }));
  }
  // Any count change in a group holding more than one distinct verdict invalidates the ordinals,
  // and it is not enough to check whether the buckets still agree. layout.css's two 640px blocks
  // proved it: both were 'pending', so a bucket-only guard stayed quiet when batch 1 deleted the
  // first -- and the survivor silently inherited item #30's verdict ("dead, shadowed by :601")
  // when it is really #32, the live one carrying the safe-area-inset expression. Same bucket,
  // wrong item, wrong note, and the note is what a reader acts on.
  if (decision.count !== group.lines.length && decision.items.length > 1) {
    return group.lines.map((line) => ({
      line,
      bucket: 'needs-readjudication',
      klass: null,
      item: null,
      note: `group had ${decision.count} occurrences when adjudicated, now ${group.lines.length};`
        + ' ordinals no longer identify which verdict belongs to which block',
    }));
  }
  return group.lines.map((line, index) => {
    const entry = decision.items[index] || decision.items[decision.items.length - 1];
    return { line, bucket: entry.bucket, klass: entry.klass, item: entry.item, note: entry.note };
  });
}

function buildBaseline() {
  const groups = [...collectOutOfContract().values()]
    .sort((a, b) => a.file.localeCompare(b.file) || a.width - b.width || a.params.localeCompare(b.params));
  return {
    note: 'Generated file -- do not hand-edit. Regenerate with: node scripts/audit/map-token-migration.mjs --write-baseline',
    generatedAt: new Date().toISOString(),
    contract: {
      allowedWidths: [...ALLOWED_BREAKPOINT_WIDTHS].sort((a, b) => a - b),
      derivedFrom: BREAKPOINT_RULES.map(([value, token]) => ({ value, token, complement: value + 1 })),
    },
    semantics: {
      approved: 'adjudicated and settled (D class B component-scoped, class C device tier). Silent.',
      pending: 'a to-do, not an exemption (D class A should converge, class E is dead code). Reported every run, does not fail.',
      matching: 'on file+params+width and occurrence count only. Lines are informational and are expected to drift.',
    },
    source: 'adjudication table BREAKPOINT_DECISIONS in scripts/audit/map-token-migration.mjs, from .run/codex-tasks/aurora-P2b-2-D.out.md section 1.1',
    groups: groups.map((group) => ({
      key: group.key,
      file: group.file,
      params: group.params,
      width: group.width,
      count: group.lines.length,
      occurrences: adjudicate(group),
    })),
  };
}

function checkBreakpointContract() {
  const groups = collectOutOfContract();
  // Denominators first and unconditionally. Everything below prints zeros when the walk found
  // nothing, and zeros from "nothing violates the contract" have to be distinguishable from zeros
  // from "no CSS was parsed". These four numbers are the only thing that makes the verdict
  // falsifiable, so they are printed before any verdict is reached.
  const totalOccurrences = [...groups.values()].reduce((sum, group) => sum + group.lines.length, 0);
  console.log(`breakpoint contract: ${breakpointScan.widthOperands} width operands`
    + ` in ${breakpointScan.mediaBlocks} @media blocks across ${breakpointScan.cssFilesWalked} css files`
    + `; allowed {${[...ALLOWED_BREAKPOINT_WIDTHS].sort((a, b) => a - b).join(',')}}`
    + `; out of contract ${totalOccurrences} in ${groups.size} groups`);

  const failures = [];
  const pending = [];

  if (!breakpointScan.cssFilesWalked || !breakpointScan.mediaBlocks || !breakpointScan.widthOperands) {
    failures.push('nothing was examined: the css walk produced no @media width operand at all,'
      + ' so a clean result here would mean the assertion never ran');
  }
  for (const entry of breakpointScan.unparsed) {
    failures.push(`${entry.source}: ${entry.widthMentions} width condition(s) but only ${entry.parsed} parsed`
      + ` -- unsupported syntax escapes the contract check: ${entry.params}`);
  }

  if (!fs.existsSync(baselinePath)) {
    failures.push(`missing ${rel(baselinePath)}; generate it with --write-baseline`);
    report_(failures, pending);
    return;
  }
  const baseline = JSON.parse(fs.readFileSync(baselinePath, 'utf8'));
  const registered = new Map((baseline.groups || []).map((group) => [group.key, group]));

  const allowedNow = [...ALLOWED_BREAKPOINT_WIDTHS].sort((a, b) => a - b).join(',');
  const allowedThen = (baseline.contract?.allowedWidths || []).join(',');
  if (allowedNow !== allowedThen) {
    failures.push(`baseline was generated against allowed widths {${allowedThen}} but the contract now admits`
      + ` {${allowedNow}}; regenerate with --write-baseline`);
  }

  for (const [key, group] of groups) {
    const known = registered.get(key);
    if (!known) {
      failures.push(`new out-of-contract breakpoint: ${group.file} ${group.params} -> ${group.width}px`
        + ` at line(s) ${group.lines.join(', ')} (not in the baseline; converge it to 767/1100 or adjudicate it)`);
      continue;
    }
    if (group.lines.length > known.count) {
      failures.push(`${group.file} ${group.params} -> ${group.width}px gained occurrences:`
        + ` baseline ${known.count}, now ${group.lines.length} at line(s) ${group.lines.join(', ')}`);
      continue;
    }
    const stillPending = (known.occurrences || []).filter((entry) => entry.bucket !== 'approved');
    if (stillPending.length) {
      pending.push(`${group.file} ${group.params} -> ${group.width}px`
        + ` [${stillPending.map((entry) => `#${entry.item ?? '?'} ${entry.klass || entry.bucket}`).join(', ')}]`
        + ` now at line(s) ${group.lines.join(', ')}`);
    }
    if (group.lines.length < known.count) {
      pending.push(`${group.file} ${group.params} -> ${group.width}px partially resolved:`
        + ` baseline ${known.count}, now ${group.lines.length} -- regenerate the baseline to retire the difference`);
    }
  }
  // A baseline entry matching nothing is the stale-baseline case the whole design is guarding
  // against: it is what a batch of deletions leaves behind, and it must be visible rather than
  // simply passing. Not fatal -- the deletion it records is the outcome we wanted.
  for (const [key, group] of registered) {
    if (groups.has(key)) continue;
    pending.push(`resolved and now stale in the baseline: ${group.file} ${group.params} -> ${group.width}px`
      + ` (was ${group.count} occurrence(s)) -- regenerate with --write-baseline`);
  }

  report_(failures, pending);
}

function report_(failures, pending) {
  if (pending.length) {
    console.log(`breakpoint contract: ${pending.length} open item(s), not failing the gate:`);
    for (const line of pending) console.log(`  - ${line}`);
  }
  if (!failures.length) {
    console.log('breakpoint contract: no new out-of-contract width');
    return;
  }
  console.error(`breakpoint contract: ${failures.length} violation(s)`);
  for (const line of failures) console.error(`  - ${line}`);
  process.exit(1);
}

if (hasFlag('--write-baseline')) {
  const baseline = buildBaseline();
  const unresolved = baseline.groups.flatMap((group) => group.occurrences
    .filter((entry) => entry.bucket === 'unclassified' || entry.bucket === 'needs-readjudication')
    .map((entry) => ({ group, entry })));
  fs.writeFileSync(baselinePath, `${JSON.stringify(baseline, null, 2)}\n`);
  const counts = baseline.groups.flatMap((group) => group.occurrences).reduce((acc, entry) => {
    acc[entry.bucket] = (acc[entry.bucket] || 0) + 1;
    return acc;
  }, {});
  console.log(`wrote ${rel(baselinePath)}: ${baseline.groups.length} groups,`
    + ` ${baseline.groups.reduce((sum, group) => sum + group.count, 0)} occurrences`
    + ` (${Object.entries(counts).map(([bucket, n]) => `${bucket} ${n}`).join(', ') || 'none'})`);
  if (unresolved.length) {
    // Writing an unadjudicated entry as though it were settled is how a to-do becomes a permanent
    // exemption. The file is written so the diff is reviewable, but the command fails.
    console.error(`${unresolved.length} occurrence(s) have no usable adjudication:`);
    for (const { group, entry } of unresolved) {
      console.error(`  - ${group.file}:${entry.line} ${group.params} -> ${group.width}px: ${entry.note}`);
    }
    console.error('Add them to BREAKPOINT_DECISIONS in this script, then regenerate.');
    process.exit(1);
  }
} else {
  checkBreakpointContract();
}
