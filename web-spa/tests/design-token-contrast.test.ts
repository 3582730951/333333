import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { contrastRatio } from '../src/lib/colorMetrics.js';

const tokenPath = ['.', 'web-spa']
  .map((root) => resolve(process.cwd(), root, 'src/styles/tokens.css'))
  .find((candidate) => existsSync(candidate));
if (!tokenPath) throw new Error('tokens.css not found');
const css = readFileSync(tokenPath, 'utf8');

// tokens.css is dark-first: dark is the :root default and light is the explicit
// opt-out, so the combined `:root,` prefix now sits on the dark block. Both themes
// stay measured -- the flip changed which selector carries the default, not the
// contract that neither theme may regress.
const selectors = {
  light: /html\[data-theme='light'\]\s*\{([\s\S]*?)\n\}/,
  dark: /:root,\s*html\[data-theme='dark'\]\s*\{([\s\S]*?)\n\}/,
} as const;

function tokens(theme: keyof typeof selectors) {
  const block = css.match(selectors[theme])?.[1];
  if (!block) throw new Error(`missing ${theme} token block`);
  return (name: string) => {
    const value = block.match(new RegExp(`--pool-${name}:\\s*(#[0-9a-fA-F]{6})\\s*;`))?.[1];
    if (!value) throw new Error(`--pool-${name} must be a literal six-digit hex in ${theme}`);
    return value;
  };
}

describe.each(['light', 'dark'] as const)('semantic contrast tokens (%s)', (theme) => {
  const read = tokens(theme);

  it('keeps primary and secondary text AA-readable', () => {
    for (const surface of ['bg-page', 'bg-surface'] as const) {
      expect(contrastRatio(read('text'), read(surface)), `text on ${surface}`).toBeGreaterThanOrEqual(4.5);
      expect(contrastRatio(read('text-2'), read(surface)), `text-2 on ${surface}`).toBeGreaterThanOrEqual(4.5);
      expect(contrastRatio(read('text-3'), read(surface)), `text-3 on ${surface}`).toBeGreaterThanOrEqual(4.5);
    }
  });

  it('keeps controls, focus, and primary actions perceivable', () => {
    expect(contrastRatio(read('control-border'), read('bg-surface')), 'control border').toBeGreaterThanOrEqual(3);
    expect(contrastRatio(read('focus-color'), read('bg-page')), 'focus on page').toBeGreaterThanOrEqual(3);
    expect(contrastRatio(read('focus-color'), read('bg-surface')), 'focus on surface').toBeGreaterThanOrEqual(3);
    expect(contrastRatio(read('action'), read('on-action')), 'primary button').toBeGreaterThanOrEqual(4.5);
  });
});
