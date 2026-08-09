import { existsSync, readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { PALETTE } from '../src/lib/chartTheme.js';
import {
  ciede2000,
  contrastRatio,
  deuteranopicDelta,
  protanopicDelta,
  simulateDichromacy,
} from '../src/lib/colorMetrics.js';

// Why this file exists.
//
// chartTheme.js carried a comment stating that "under simulated deuteranopia the orange and
// red slots fall to dE ~3", offered as a limitation that could not be fixed because those
// were "the semantic warning/danger hues". Both halves were wrong by the time anyone read it:
//
//   * --chart-orange resolves to --pool-chart-orange, a dedicated chart token -- not
//     --pool-warning, and not since the palette stopped borrowing semantic tokens. Retuning
//     it changes no status colour anywhere, so the stated blocker did not exist.
//   * orange/red actually measures dE 8.1 (light) and above 10 (dark). The real severe
//     collision was blue/purple at dE 1.8 in the light theme: two slots a two-series chart
//     can draw together, rendering as one colour for a deuteranope.
//
// Those numbers came from a script that was never committed, so nothing re-ran them and they
// rotted while still reading as authoritative. This file is the re-measurement. It parses the
// shipped stylesheet rather than a copied table of hexes, because a hand-copied palette would
// keep passing while tokens.css drifted -- which is the same failure one level up.

// Read off disk, not through Vite, and not from cwd alone. Three routes failed first:
//
//   * `import '../src/styles/tokens.css?raw'` returns an empty string -- vitest defaults to
//     css:false and stubs the CSS pipeline, so ?raw resolves to nothing at all;
//   * `fileURLToPath(import.meta.url)` throws, because under the jsdom environment
//     import.meta.url is an http:// URL rather than a file:// one;
//   * `resolve(process.cwd(), ...)` works when vitest is invoked from web-spa/ and breaks when
//     it is invoked from the repo root, which is how the check scripts run it.
//
// So try both roots and say which were tried if neither has it. A missing stylesheet has to be
// an error and never an empty read: this file's whole purpose is that the numbers are measured
// against what ships, and measuring an empty string would pass every assertion vacuously.
const CANDIDATE_ROOTS = ['.', 'web-spa'];
const TOKENS_PATH = CANDIDATE_ROOTS
  .map((root) => resolve(process.cwd(), root, 'src/styles/tokens.css'))
  .find((candidate) => existsSync(candidate));
if (!TOKENS_PATH) {
  throw new Error(
    `tokens.css not found from cwd ${process.cwd()}; tried ${CANDIDATE_ROOTS.join(', ')}`,
  );
}
const tokensCss = readFileSync(TOKENS_PATH, 'utf8');

const SLOTS = ['blue', 'green', 'purple', 'orange', 'teal', 'red', 'indigo', 'pink'] as const;

// tokens.css declares dark under `:root, html[data-theme='dark']` and light under
// `html[data-theme='light']`. Both blocks are read so neither theme can regress unmeasured.
const THEME_SELECTORS = {
  dark: /:root,\s*html\[data-theme='dark'\]\s*\{([\s\S]*?)\n\}/,
  light: /html\[data-theme='light'\]\s*\{([\s\S]*?)\n\}/,
} as const;

function readTheme(theme: keyof typeof THEME_SELECTORS) {
  const block = tokensCss.match(THEME_SELECTORS[theme]);
  if (!block) throw new Error(`could not locate the ${theme} block in tokens.css`);
  const body = block[1];
  const read = (name: string) => {
    const found = body.match(new RegExp(`--${name}:\\s*(#[0-9a-fA-F]{3,6})\\s*;`));
    if (!found) throw new Error(`--${name} is not a literal hex in the ${theme} block`);
    return found[1].toLowerCase();
  };
  return {
    background: read('pool-bg-elevated'),
    hues: Object.fromEntries(SLOTS.map((s) => [s, read(`pool-chart-${s}`)])) as Record<string, string>,
  };
}

// 12 is the separation the palette was solved for under normal vision and stays a hard floor.
// Dichromatic vision collapses the space to roughly two dimensions (blue-yellow plus lightness),
// so eight vivid, name-honest hues cannot all clear 12 there -- see the worst-case floors at the
// bottom of this file for what they actually reach. CVD_SEVERE is the weaker claim that no two
// slots are *the same colour*, which is roughly where CIEDE2000 puts a just-noticeable
// difference; anything under it means a reader sees one line where the legend names two.
const NORMAL_FLOOR = 12;
const CVD_SEVERE = 5;

describe.each(['dark', 'light'] as const)('chart palette (%s)', (theme) => {
  const { hues, background } = readTheme(theme);

  it('declares a literal hex for every palette slot', () => {
    // A slot added to PALETTE without a token here would ship entirely unmeasured.
    expect(Object.keys(hues).length).toBe(PALETTE.length);
    for (const [name, hex] of Object.entries(hues)) expect(hex, name).toMatch(/^#[0-9a-f]{6}$/);
  });

  it('keeps every pair separable under normal vision', () => {
    const tooClose: string[] = [];
    for (let i = 0; i < SLOTS.length; i++) {
      for (let j = i + 1; j < SLOTS.length; j++) {
        const dE = ciede2000(hues[SLOTS[i]], hues[SLOTS[j]]);
        if (dE < NORMAL_FLOOR) tooClose.push(`${SLOTS[i]}/${SLOTS[j]} dE ${dE.toFixed(1)}`);
      }
    }
    expect(tooClose, `pairs below dE ${NORMAL_FLOOR}`).toEqual([]);
  });

  // Both dichromacies, because checking only one is how the second defect survived. The light
  // purple was first "fixed" by rotating hue 251 -> 284 at the same lightness, which took
  // deuteranopic blue/purple from 1.8 to 6.0 and pushed the *protanopic* pair from 3.5 down to
  // 0.6 -- a worse collision than the one being fixed, in the direction nobody measured. A
  // protanope separates purple from blue almost entirely by lightness, so hue rotation alone
  // cannot help; both current purples are darker/lighter than their predecessors, not just
  // rotated. Deuteranopia is the more common condition but neither is a rounding error.
  it.each([
    ['deuteranope', deuteranopicDelta, 'deuteranopia'],
    ['protanope', protanopicDelta, 'protanopia'],
  ] as const)('has no pair a %s sees as a single colour', (_who, delta, kind) => {
    const merged: string[] = [];
    for (let i = 0; i < SLOTS.length; i++) {
      for (let j = i + 1; j < SLOTS.length; j++) {
        const dE = delta(hues[SLOTS[i]], hues[SLOTS[j]]);
        if (dE < CVD_SEVERE) {
          merged.push(`${SLOTS[i]}/${SLOTS[j]} dE ${dE.toFixed(1)} `
            + `(${simulateDichromacy(hues[SLOTS[i]], kind)} vs ${simulateDichromacy(hues[SLOTS[j]], kind)})`);
        }
      }
    }
    expect(merged, `pairs a ${kind} viewer cannot separate (dE < ${CVD_SEVERE})`).toEqual([]);
  });

  it('holds every slot at 3:1 on the card background', () => {
    // WCAG 2.2 SC 1.4.11 -- these draw as 2px strokes and 9px legend dots, so they are
    // non-text UI components rather than decoration.
    for (const [name, hex] of Object.entries(hues)) {
      expect(contrastRatio(hex, background), `${name} ${hex} on ${background}`).toBeGreaterThanOrEqual(3);
    }
  });

  it('keeps the slots within one lightness family', () => {
    // A slot can satisfy every dE floor above by leaving the palette's lightness range
    // entirely: solving the light-theme purple numerically first returned #281858 at 15.4:1
    // against white, which reads as near-black rather than as a hue. Bounding the spread
    // rather than an absolute ceiling is what makes this survive honest colours -- dark green
    // and orange sit at 8.3 and 8.5 legitimately, while that outlier was a 5x spread.
    const ratios = Object.values(hues).map((hex) => contrastRatio(hex, background));
    const spread = Math.max(...ratios) / Math.min(...ratios);
    expect(spread, `contrast spread ${spread.toFixed(1)}x across the palette`).toBeLessThanOrEqual(3.5);
  });
});

describe('dichromatic worst case', () => {
  // Deliberately NOT "how many slots can be told apart", which was the first version of this
  // assertion and was measuring the wrong thing. That metric asks for the largest mutually
  // separable subset, and the old light palette scored 5 on it -- by choosing a subset that
  // excluded blue, the very slot in its dE 1.8 collision. seriesColorMap hashes model keys into
  // slots, so a chart cannot choose its subset; it gets whichever pair the hash produces. The
  // number that governs what a reader actually sees is therefore the worst pair that can
  // co-occur, which is every pair.
  //
  // These floors are what the palette measures today, recorded so a regression is visible as a
  // number rather than as prose that can rot. They are low: dichromats perceive roughly two
  // dimensions, so eight vivid hues genuinely cannot all separate at dE 12, and desaturating
  // until they did would cost every other reader a name-honest palette. That is the reason
  // charts must keep a text legend or direct label -- colour is never the only channel.
  // Set from the measured values, not from their rounded display -- the first draft of this
  // table read 6.9 off a `.toFixed(1)` and failed against the true 6.8516. Each floor sits a
  // hair under what the palette reaches, so honest float drift does not fail the suite but a
  // real regression does. Binding pairs are all pink-adjacent; purple is no longer among them.
  const FLOORS = {
    light: { deuteranopia: 6.0, protanopia: 6.5 },   // orange/pink 6.02, green/pink 6.57
    dark: { deuteranopia: 6.8, protanopia: 7.1 },    // red/pink 6.85, purple/teal 7.13
  } as const;

  it.each(['dark', 'light'] as const)('records the closest %s pair under each dichromacy', (theme) => {
    const { hues } = readTheme(theme);
    for (const [kind, delta] of [
      ['deuteranopia', deuteranopicDelta],
      ['protanopia', protanopicDelta],
    ] as const) {
      let worst = Infinity;
      let pair = '';
      for (let i = 0; i < SLOTS.length; i++) {
        for (let j = i + 1; j < SLOTS.length; j++) {
          const dE = delta(hues[SLOTS[i]], hues[SLOTS[j]]);
          if (dE < worst) {
            worst = dE;
            pair = `${SLOTS[i]}/${SLOTS[j]}`;
          }
        }
      }
      expect(worst, `${theme} closest ${kind} pair is ${pair} at dE ${worst.toFixed(1)}`)
        .toBeGreaterThanOrEqual(FLOORS[theme][kind]);
    }
  });
});
