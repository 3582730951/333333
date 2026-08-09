import { describe, expect, it } from 'vitest';
import { PALETTE, COLORS, modelColor, seriesColorMap } from '../src/lib/chartTheme.js';

// The palette was nominally 8 slots but carried only 6 distinguishable colours, because its
// last three slots borrowed general-purpose semantic tokens: in the light theme
// --pool-info resolved to the exact same value as --chart-blue in slot 0. On top of that,
// `modelColor` hashed keys into it with `PALETTE[hash % 8]`, so with 12 realistic model keys
// the charts drew 12 series in 6 colours across 5 colliding slots -- three models
// (gpt-5-codex, claude-sonnet-4, gemini-2.5-pro) all rendered in the same green, which makes
// a legend unable to say which line is which.
//
// These cover the invariant rather than specific colours: whatever the keys, two series on
// one chart must not share a colour while the palette has room.

// The 12 keys that produced the original measurement.
const REAL_KEYS = [
  'gpt-5-codex', 'claude-sonnet-4', 'gemini-2.5-pro', 'claude-opus-4', 'gpt-4o',
  'claude-haiku-4-5', 'claude-3-7-sonnet', 'gemini-2.5-flash', 'o3', 'codex-default',
  'o4-mini', 'claude-sonnet-4-5',
];

describe('chart palette', () => {
  it('has no duplicate slots', () => {
    expect(new Set(PALETTE).size).toBe(PALETTE.length);
  });

  it('draws every slot from a dedicated --chart-* token', () => {
    // Borrowing --pool-* semantic tokens is what produced the duplicates. A slot pointing at
    // a general-purpose token can be retuned for its semantic role and silently collide.
    for (const slot of PALETTE) {
      expect(slot).toMatch(/^var\(--chart-[a-z]+\)$/);
    }
  });

  it('resolves every named colour to a dedicated token, not a palette index', () => {
    // COLORS used to alias slot positions (`cyan: PALETTE[4]`), which meant reordering the
    // palette moved the colour of every semantic MetricCard.
    for (const [name, value] of Object.entries(COLORS)) {
      expect(value, name).toMatch(/^var\(--chart-[a-z]+\)$/);
    }
  });

  it('gives cyan and blue different tokens', () => {
    // These two co-occur on Dashboard (cooling vs Codex), System (network vs Go process) and
    // Usage (hit rate vs requests). In the light theme they were the same hex.
    expect(COLORS.cyan).not.toBe(COLORS.blue);
  });
});

describe('seriesColorMap', () => {
  it('gives distinct colours to distinct keys up to palette size', () => {
    for (let n = 1; n <= PALETTE.length; n++) {
      const keys = REAL_KEYS.slice(0, n);
      const colorOf = seriesColorMap(keys);
      const used = keys.map(colorOf);
      expect(new Set(used).size, `${n} keys`).toBe(n);
    }
  });

  it('resolves the 12 keys that previously collapsed to 6 colours', () => {
    const colorOf = seriesColorMap(REAL_KEYS);
    // 12 keys cannot all differ in an 8-slot palette, but every slot must now be in play --
    // the hash alone reached only 6 of them.
    expect(new Set(REAL_KEYS.map(colorOf)).size).toBe(PALETTE.length);
  });

  it('splits the three models that used to share one green', () => {
    // The measured worst case: gpt-5-codex, claude-sonnet-4 and gemini-2.5-pro all hashed to
    // the same slot, so a chart showing all three drew three lines in one colour.
    const trio = ['gpt-5-codex', 'claude-sonnet-4', 'gemini-2.5-pro'];
    expect(new Set(trio.map(modelColor)).size).toBe(1);      // the defect, still reproducible
    const colorOf = seriesColorMap(trio);
    expect(new Set(trio.map(colorOf)).size).toBe(3);         // and now resolved
  });

  it('gives every key a distinct colour for each real collision group', () => {
    // The other four measured groups.
    for (const group of [
      ['claude-opus-4', 'gpt-4o'],
      ['claude-haiku-4-5', 'claude-3-7-sonnet'],
      ['gemini-2.5-flash', 'o3'],
      ['codex-default', 'o4-mini'],
    ]) {
      const colorOf = seriesColorMap(group);
      expect(new Set(group.map(colorOf)).size, group.join('+')).toBe(group.length);
    }
  });

  it('is deterministic across calls', () => {
    const a = seriesColorMap(REAL_KEYS);
    const b = seriesColorMap(REAL_KEYS);
    for (const k of REAL_KEYS) expect(b(k)).toBe(a(k));
  });

  it('keeps a key on its hashed slot when nothing contends for it', () => {
    // This is what preserves colour stability for a model across different pages: probing is
    // a fallback, not the normal path.
    for (const key of REAL_KEYS) {
      expect(seriesColorMap([key])(key)).toBe(modelColor(key));
    }
  });

  it('does not let an earlier key displace a later key from its free hashed slot', () => {
    // Pass 1 hands out uncontested slots before any probing, so a probing key cannot land on
    // a slot that some later key would have taken uncontested.
    const colorOf = seriesColorMap(REAL_KEYS.slice(0, 8));
    for (const key of REAL_KEYS.slice(0, 8)) {
      const preferred = modelColor(key);
      const others = REAL_KEYS.slice(0, 8).filter((k) => k !== key);
      const contested = others.some((k) => modelColor(k) === preferred);
      if (!contested) expect(colorOf(key), key).toBe(preferred);
    }
  });

  it('tolerates duplicate keys without consuming extra slots', () => {
    const colorOf = seriesColorMap(['a', 'a', 'b', 'b', 'b']);
    expect(colorOf('a')).toBe(colorOf('a'));
    expect(new Set(['a', 'b'].map(colorOf)).size).toBe(2);
  });

  it('still returns a palette colour when there are more series than slots', () => {
    const many = Array.from({ length: PALETTE.length + 5 }, (_, i) => `model-${i}`);
    const colorOf = seriesColorMap(many);
    for (const k of many) expect(PALETTE).toContain(colorOf(k));
    // All slots get used before any is reused.
    expect(new Set(many.map(colorOf)).size).toBe(PALETTE.length);
  });

  it('handles empty, null and undefined input without throwing', () => {
    for (const input of [[], null, undefined] as any[]) {
      const colorOf = seriesColorMap(input);
      expect(PALETTE).toContain(colorOf('anything'));
    }
  });

  it('falls back to a palette colour for a key it was not built with', () => {
    const colorOf = seriesColorMap(['a', 'b']);
    expect(PALETTE).toContain(colorOf('never-registered'));
  });

  it('treats null and undefined keys as one empty key', () => {
    const colorOf = seriesColorMap([null, undefined, '']);
    expect(colorOf(null as any)).toBe(colorOf(''));
    expect(colorOf(undefined as any)).toBe(colorOf(''));
  });
});
