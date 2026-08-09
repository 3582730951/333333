// Chart colour system.
//
// Every colour here is a dedicated `--chart-*` token. The palette used to borrow three
// general-purpose semantic tokens (--pool-info, --pool-purple, --pool-success) to pad itself
// out to eight slots, and that borrowing was measurably broken:
//
//   * light theme: slot 0 (--chart-blue) and slot 4 (--pool-info) resolved to byte-identical
//     values, so two different series could render in literally the same colour;
//   * both themes: slot 1 (--chart-green) vs slot 7 (--pool-success) sat at CIEDE2000
//     dE 5.0 (dark) / 5.8 (light);
//   * dark theme: slot 2 (--chart-purple) vs slot 6 (--pool-purple) sat at dE 5.5.
//
// So the nominally 8-slot palette carried only 6 mutually distinguishable colours, and
// `modelColor` was hashing into it -- with 12 realistic model keys that produced 6 distinct
// colours across 5 colliding slots. Two models drawn in one colour makes the legend unable
// to name a line.
//
// The eight slots below are now all dedicated chart hues, verified pairwise at dE >= 15.1
// (light) / 12.5 (dark) under normal vision, and >= 3:1 contrast against the card background
// (WCAG 2.2 SC 1.4.11, which governs these 2px strokes and 9px legend dots).
//
// Colour-vision limits are measured rather than asserted, in tests/chart-color-vision.test.ts,
// which parses the shipped tokens.css on every run. That file exists because the paragraph it
// replaced was wrong in three ways at once: it claimed orange/red collapsed to dE ~3 under
// deuteranopia (it is 8.1 in light), claimed the cause was those being the semantic
// warning/danger hues (they are dedicated --pool-chart-* tokens no status colour reads), and
// missed the actual collisions entirely. Nothing re-ran its numbers, so they rotted in place.
//
// The two real defects it hid, both since fixed by retuning --pool-chart-purple in each theme:
//
//   * light blue/purple at deuteranopic dE 1.8 -- one colour for a red-green colour-blind
//     reader, and reachable on a two-series chart because seriesColorMap hashes into slots;
//   * dark blue/purple at *protanopic* dE 1.2, worse still, and invisible to an audit that
//     simulated only deuteranopia. Protanopes separate purple from blue almost entirely by
//     lightness, so rotating hue does not help them -- the first attempted fix rotated light
//     purple at constant lightness and drove its protanopic pair from 3.5 down to 0.6.
//
// What remains is a property of dichromatic vision, not a tuning failure. Dichromats perceive
// roughly two dimensions (blue-yellow plus lightness), so eight vivid, name-honest hues cannot
// all reach the dE 12 used for normal vision; the palette's closest pairs sit near 6 (light)
// and 6.9 (dark), all of them pink-adjacent. Desaturating until they cleared 12 would cost
// every other reader a palette whose colours match their names and still not fit eight slots.
// So colour is never the only channel here: every chart carries a text legend or a direct label.
export const PALETTE = [
  'var(--chart-blue)',
  'var(--chart-green)',
  'var(--chart-purple)',
  'var(--chart-orange)',
  'var(--chart-teal)',
  'var(--chart-red)',
  'var(--chart-indigo)',
  'var(--chart-pink)',
];

// Named for meaning, resolved straight to tokens rather than to PALETTE indices. Previously
// these were aliases like `cyan: PALETTE[4]`, which tied the semantic name to a slot
// position: reordering the palette silently moved every MetricCard's colour.
export const COLORS = {
  blue: 'var(--chart-blue)',
  green: 'var(--chart-green)',
  violet: 'var(--chart-purple)',
  amber: 'var(--chart-orange)',
  cyan: 'var(--chart-cyan)',
  red: 'var(--chart-red)',
  pink: 'var(--chart-pink)',
  teal: 'var(--chart-teal)',
  grey: 'var(--chart-gray)',
};

function hashKey(name) {
  const s = String(name ?? '');
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return h;
}

// Stable colour for one key, with no knowledge of what else is on the chart. Use this only
// where a single value is coloured in isolation; anything drawing a *set* of series should
// use seriesColorMap, which cannot hand the same colour to two keys.
export function modelColor(name) {
  return PALETTE[hashKey(name) % PALETTE.length];
}

// Collision-free colour assignment for a set of series.
//
// The hashed slot is only a *preference*: a key takes it when free, otherwise it probes
// forward to the next free slot. That keeps the common case (no contention) identical to
// modelColor -- so a model tends to keep its colour across pages -- while guaranteeing that
// two keys on the same chart never share one, as long as the set fits in the palette.
//
// Assignment depends on the order of `keys`, which is stable because callers pass the
// API's own ordering. Returns a lookup function; unknown keys fall back to modelColor.
export function seriesColorMap(keys) {
  const assigned = new Map();
  const taken = new Array(PALETTE.length).fill(false);

  const unique = [];
  for (const raw of keys || []) {
    const key = String(raw ?? '');
    if (!assigned.has(key)) {
      assigned.set(key, null);
      unique.push(key);
    }
  }

  // Pass 1: hand out uncontested hashed slots. Done before any probing so that a key whose
  // preferred slot is free is never displaced by an earlier key probing into it.
  const contended = [];
  for (const key of unique) {
    const slot = hashKey(key) % PALETTE.length;
    if (taken[slot]) contended.push(key);
    else {
      taken[slot] = true;
      assigned.set(key, PALETTE[slot]);
    }
  }

  // Pass 2: probe forward for the rest.
  for (const key of contended) {
    const start = hashKey(key) % PALETTE.length;
    let placed = false;
    for (let step = 1; step <= PALETTE.length; step++) {
      const slot = (start + step) % PALETTE.length;
      if (taken[slot]) continue;
      taken[slot] = true;
      assigned.set(key, PALETTE[slot]);
      placed = true;
      break;
    }
    // More series than slots. Reuse is unavoidable at that point; fall back to the hashed
    // slot so the result stays deterministic instead of depending on probe order.
    if (!placed) assigned.set(key, PALETTE[start]);
  }

  return (key) => assigned.get(String(key ?? '')) ?? modelColor(key);
}
