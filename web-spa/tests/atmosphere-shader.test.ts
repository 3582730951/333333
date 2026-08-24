import { describe, expect, it } from 'vitest';
import {
  FRAGMENT_SOURCE, UNIFORM_NAMES, VERTEX_SOURCE, parseColorChannels,
} from '../src/lib/atmosphere.js';

// Why this file exists.
//
// The atmosphere layer is built to degrade silently: no WebGL2, or a program that
// will not link, and the caller keeps the CSS field. That is correct behaviour and
// it is also the perfect place to hide a defect. A missing `in vec2 vUv;` in the
// fragment stage failed every compile for an entire development cycle while
// presenting exactly as "this browser has no WebGL" -- the canvas sat at its
// default 300x150, painted nothing, and no gate, typecheck or screenshot noticed,
// because the fallback underneath looked plausible.
//
// GLSL cannot be compiled here (jsdom has no GL). What can be checked is every
// cross-stage contract that a compiler would have caught, which is where that bug
// actually lived.

const declarations = (source: string, qualifier: 'in' | 'out' | 'uniform') => (
  [...source.matchAll(new RegExp(`^\\s*${qualifier}\\s+(\\w+)\\s+(\\w+)\\s*;`, 'gm'))]
    .map((match) => `${match[1]} ${match[2]}`)
);

describe('atmosphere shader', () => {
  it('declares every vertex output as a fragment input', () => {
    // Type and name both, because `out vec2 vUv` paired with `in float vUv` is a
    // link error rather than a compile error and would fail one stage later.
    const outputs = declarations(VERTEX_SOURCE, 'out');
    const inputs = declarations(FRAGMENT_SOURCE, 'in');
    expect(outputs.length).toBeGreaterThan(0);
    expect(inputs, 'fragment inputs must mirror vertex outputs').toEqual(expect.arrayContaining(outputs));
  });

  it('names every fragment uniform in the JavaScript uniform list', () => {
    // A uniform the shader declares but the host never looks up is silently never
    // set, so it reads as zero and the effect it drives quietly disappears.
    const declared = declarations(FRAGMENT_SOURCE, 'uniform').map((entry) => entry.split(' ')[1]);
    expect(declared.length).toBeGreaterThan(0);
    for (const name of declared) expect(UNIFORM_NAMES, `${name} is not looked up`).toContain(name);
    for (const name of UNIFORM_NAMES) expect(declared, `${name} is looked up but not declared`).toContain(name);
  });

  it('both stages declare a version and the fragment stage a precision', () => {
    expect(VERTEX_SOURCE.trimStart().startsWith('#version 300 es')).toBe(true);
    expect(FRAGMENT_SOURCE.trimStart().startsWith('#version 300 es')).toBe(true);
    // GLSL ES 3.00 has no default float precision in the fragment stage.
    expect(FRAGMENT_SOURCE).toMatch(/precision\s+(?:lowp|mediump|highp)\s+float\s*;/);
  });

  it('writes to a declared fragment output', () => {
    const outputs = declarations(FRAGMENT_SOURCE, 'out').map((entry) => entry.split(' ')[1]);
    expect(outputs.length).toBe(1);
    expect(FRAGMENT_SOURCE).toContain(`${outputs[0]} =`);
  });

  it('carries no colour literal of its own', () => {
    // The palette is handed in from tokens.css so the field follows the theme.
    // Mirrors what check:pool-ui-migration enforces across the tree, asserted here
    // too because a hex inside a GLSL string is the easy place to reintroduce one.
    for (const source of [VERTEX_SOURCE, FRAGMENT_SOURCE]) {
      expect(source).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    }
  });
});

describe('parseColorChannels', () => {
  it('reads the two forms tokens.css resolves to', () => {
    expect(parseColorChannels('#ffffff')).toEqual([1, 1, 1]);
    expect(parseColorChannels('#000')).toEqual([0, 0, 0]);
    // Browsers resolve a token to a colour function body; only the first three
    // numbers are channels, and a trailing alpha percentage must not become one.
    const parsed = parseColorChannels('rgb(20 25 32 / 58%)');
    expect(parsed).not.toBeNull();
    expect(parsed).toHaveLength(3);
    expect(parsed![0]).toBeCloseTo(20 / 255, 5);
    expect(parsed![2]).toBeCloseTo(32 / 255, 5);
  });

  it('returns null rather than a wrong colour for anything it cannot read', () => {
    // The caller keeps the previous uniform on null; returning a default here
    // would flash an unintended colour on every theme change that races the read.
    for (const value of ['', '   ', 'not-a-colour', '#12', undefined, null]) {
      expect(parseColorChannels(value as string), String(value)).toBeNull();
    }
  });
});
