#!/usr/bin/env node
/** Aurora P0 contrast and two-dichromacy measurement from shipped CSS tokens. */
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import postcss from 'postcss';
import {
  ciede2000, contrastRatio, deuteranopicDelta, protanopicDelta,
} from '../../src/lib/colorMetrics.js';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const tokenFile = path.join(root, 'src', 'styles', 'tokens.css');
const slots = ['blue', 'green', 'purple', 'orange', 'teal', 'red', 'indigo', 'pink'];

function readArg(name) {
  const index = process.argv.indexOf(name);
  return index === -1 ? null : process.argv[index + 1] || null;
}

function themeDeclarations(theme, css) {
  const selector = theme === 'dark' ? ':root,\nhtml[data-theme=\'dark\']' : "html[data-theme='light']";
  const rule = css.nodes.find((node) => node.type === 'rule' && node.selector === selector);
  if (!rule) throw new Error(`missing ${theme} token rule`);
  return Object.fromEntries(rule.nodes
    .filter((node) => node.type === 'decl' && node.prop.startsWith('--pool-'))
    .map((node) => [node.prop, node.value.trim()]));
}

function minPair(hues, metric) {
  let result = { value: Infinity, pair: '' };
  for (let left = 0; left < slots.length; left += 1) {
    for (let right = left + 1; right < slots.length; right += 1) {
      const value = metric(hues[slots[left]], hues[slots[right]]);
      if (value < result.value) result = { value, pair: `${slots[left]}/${slots[right]}` };
    }
  }
  return { ...result, value: Number(result.value.toFixed(4)) };
}

const css = postcss.parse(fs.readFileSync(tokenFile, 'utf8'), { from: tokenFile });
const themes = Object.fromEntries(['dark', 'light'].map((theme) => {
  const values = themeDeclarations(theme, css);
  const contrast = [];
  for (const text of ['--pool-text', '--pool-text-2', '--pool-text-3']) {
    for (const surface of ['--pool-bg-page', '--pool-bg-surface']) {
      contrast.push({ text, surface, ratio: Number(contrastRatio(values[text], values[surface]).toFixed(3)) });
    }
  }
  contrast.push(
    { text: '--pool-control-border', surface: '--pool-bg-surface', ratio: Number(contrastRatio(values['--pool-control-border'], values['--pool-bg-surface']).toFixed(3)) },
    { text: '--pool-focus-color', surface: '--pool-bg-page', ratio: Number(contrastRatio(values['--pool-focus-color'], values['--pool-bg-page']).toFixed(3)) },
    { text: '--pool-focus-color', surface: '--pool-bg-surface', ratio: Number(contrastRatio(values['--pool-focus-color'], values['--pool-bg-surface']).toFixed(3)) },
    { text: '--pool-action', surface: '--pool-on-action', ratio: Number(contrastRatio(values['--pool-action'], values['--pool-on-action']).toFixed(3)) },
  );
  const hues = Object.fromEntries(slots.map((slot) => [slot, values[`--pool-chart-${slot}`]]));
  return [theme, {
    contrast,
    contrastMinimum: contrast.reduce((minimum, item) => item.ratio < minimum.ratio ? item : minimum, contrast[0]),
    charts: {
      normal: minPair(hues, ciede2000),
      deuteranopia: minPair(hues, deuteranopicDelta),
      protanopia: minPair(hues, protanopicDelta),
      minimumStrokeContrast: Number(Math.min(...Object.values(hues).map((hex) => contrastRatio(hex, values['--pool-bg-elevated']))).toFixed(3)),
    },
  }];
}));

const output = { generatedAt: new Date().toISOString(), tokenFile: 'src/styles/tokens.css', themes };
const out = readArg('--out');
const text = `${JSON.stringify(output, null, 2)}\n`;
if (out) {
  fs.mkdirSync(path.dirname(path.resolve(out)), { recursive: true });
  fs.writeFileSync(out, text);
} else process.stdout.write(text);
console.error(`Aurora P0 colour: dark/light AA and protanopia+deuteranopia; output ${out || 'stdout'}`);
