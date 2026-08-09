// Perceptual colour metrics: CIEDE2000, WCAG contrast, and dichromat simulation.
//
// These exist because the chart palette's distinguishability claims were previously computed
// by a throwaway script and recorded as prose in a comment. Nothing re-ran them, so when the
// palette changed the numbers silently became wrong -- the comment in chartTheme.js named
// orange/red as the worst deuteranopic pair at "dE ~3" when the real worst was blue/purple at
// dE 1.8, and justified leaving it alone with a constraint (that those were the semantic
// warning/danger tokens) that had stopped being true. Putting the maths in the source tree
// makes the claims executable, so tests/chart-color-vision.test.ts can re-measure the
// shipped stylesheet instead of trusting a comment.
//
// Pure functions over hex strings, no DOM. Safe to import from a test or a build script.

const srgbToLinear = (c) => (c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4);
const linearToSrgb = (c) => (c <= 0.0031308 ? c * 12.92 : 1.055 * c ** (1 / 2.4) - 0.055);

/**
 * Parses three- or six-digit hex into components **normalised to 0-1**, not 0-255 -- the whole
 * module works in that range, and `rgbToHex` expects it back. Worth stating because white
 * parsing to `[1, 1, 1]` reads like a parse bug if you assume byte values.
 *
 * (No literal hex in this comment on purpose: check:pool-ui-migration rejects colour literals
 * anywhere outside styles/tokens.css, and it does not exempt comments.)
 */
export function hexToRgb(hex) {
  const h = String(hex).replace('#', '').trim();
  const full = h.length === 3 ? h.split('').map((c) => c + c).join('') : h;
  if (!/^[0-9a-f]{6}$/i.test(full)) throw new Error(`not a hex colour: ${hex}`);
  return [0, 2, 4].map((i) => parseInt(full.slice(i, i + 2), 16) / 255);
}

export function rgbToHex(rgb) {
  return `#${rgb.map((c) => Math.round(Math.min(1, Math.max(0, c)) * 255).toString(16).padStart(2, '0')).join('')}`;
}

// --- WCAG 2.2 relative luminance and contrast -------------------------------------------

export function relativeLuminance(hex) {
  const [r, g, b] = hexToRgb(hex).map(srgbToLinear);
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

export function contrastRatio(hex1, hex2) {
  const a = relativeLuminance(hex1);
  const b = relativeLuminance(hex2);
  return (Math.max(a, b) + 0.05) / (Math.min(a, b) + 0.05);
}

// --- CIE Lab ----------------------------------------------------------------------------

const D65 = [0.95047, 1, 1.08883];

function rgbToXyz(rgb) {
  const [r, g, b] = rgb.map(srgbToLinear);
  return [
    r * 0.4124564 + g * 0.3575761 + b * 0.1804375,
    r * 0.2126729 + g * 0.7151522 + b * 0.0721750,
    r * 0.0193339 + g * 0.1191920 + b * 0.9503041,
  ];
}

export function hexToLab(hex) {
  const f = (t) => (t > 216 / 24389 ? Math.cbrt(t) : (841 / 108) * t + 4 / 29);
  const [fx, fy, fz] = rgbToXyz(hexToRgb(hex)).map((v, i) => f(v / D65[i]));
  return [116 * fy - 16, 500 * (fx - fy), 200 * (fy - fz)];
}

// --- CIEDE2000 --------------------------------------------------------------------------
//
// Preferred over plain Lab euclidean distance because the palette's tightest pairs sit in
// the blue-violet region, where CIE76 badly overstates separation.

export function ciede2000(hex1, hex2) {
  const [L1, a1, b1] = hexToLab(hex1);
  const [L2, a2, b2] = hexToLab(hex2);
  const C1 = Math.hypot(a1, b1);
  const C2 = Math.hypot(a2, b2);
  const Cbar = (C1 + C2) / 2;
  const G = 0.5 * (1 - Math.sqrt(Cbar ** 7 / (Cbar ** 7 + 25 ** 7)));
  const ap1 = a1 * (1 + G);
  const ap2 = a2 * (1 + G);
  const Cp1 = Math.hypot(ap1, b1);
  const Cp2 = Math.hypot(ap2, b2);
  const deg = (rad) => ((rad * 180) / Math.PI + 360) % 360;
  const hp1 = Cp1 === 0 ? 0 : deg(Math.atan2(b1, ap1));
  const hp2 = Cp2 === 0 ? 0 : deg(Math.atan2(b2, ap2));

  const dLp = L2 - L1;
  const dCp = Cp2 - Cp1;
  let dhp = 0;
  if (Cp1 * Cp2 !== 0) {
    dhp = hp2 - hp1;
    if (dhp > 180) dhp -= 360;
    else if (dhp < -180) dhp += 360;
  }
  const dHp = 2 * Math.sqrt(Cp1 * Cp2) * Math.sin((dhp * Math.PI) / 360);

  const Lbp = (L1 + L2) / 2;
  const Cbp = (Cp1 + Cp2) / 2;
  let hbp = hp1 + hp2;
  if (Cp1 * Cp2 !== 0) {
    if (Math.abs(hp1 - hp2) > 180) hbp += hbp < 360 ? 360 : -360;
    hbp /= 2;
  }

  const T = 1
    - 0.17 * Math.cos(((hbp - 30) * Math.PI) / 180)
    + 0.24 * Math.cos((2 * hbp * Math.PI) / 180)
    + 0.32 * Math.cos(((3 * hbp + 6) * Math.PI) / 180)
    - 0.20 * Math.cos(((4 * hbp - 63) * Math.PI) / 180);
  const dTheta = 30 * Math.exp(-(((hbp - 275) / 25) ** 2));
  const Rc = 2 * Math.sqrt(Cbp ** 7 / (Cbp ** 7 + 25 ** 7));
  const Sl = 1 + (0.015 * (Lbp - 50) ** 2) / Math.sqrt(20 + (Lbp - 50) ** 2);
  const Sc = 1 + 0.045 * Cbp;
  const Sh = 1 + 0.015 * Cbp * T;
  const Rt = -Math.sin((2 * dTheta * Math.PI) / 180) * Rc;

  return Math.sqrt(
    (dLp / Sl) ** 2 + (dCp / Sc) ** 2 + (dHp / Sh) ** 2 + Rt * (dCp / Sc) * (dHp / Sh),
  );
}

// --- Dichromat simulation ---------------------------------------------------------------
//
// Viénot, Brettel & Mollon (1999): sRGB -> LMS, project onto the dichromat's confusion
// plane, back to sRGB. The single-plane form is accurate for protanopia and deuteranopia,
// whose confusion lines converge on one copunctal point. Tritanopia needs the two-plane
// Brettel method and is deliberately not offered here rather than offered wrongly.

const RGB_TO_LMS = [
  [0.31399022, 0.63951294, 0.04649755],
  [0.15537241, 0.75789446, 0.08670142],
  [0.01775239, 0.10944209, 0.87256922],
];
const LMS_TO_RGB = [
  [5.47221206, -4.64196010, 0.16963708],
  [-1.12524190, 2.29317094, -0.16789520],
  [0.02980165, -0.19318073, 1.16364789],
];
const PROJECTIONS = {
  // M cone absent: reconstructed from L and S.
  deuteranopia: [
    [1, 0, 0],
    [0.9513092, 0, 0.04264227],
    [0, 0, 1],
  ],
  // L cone absent: reconstructed from M and S.
  protanopia: [
    [0, 1.05118294, -0.05116099],
    [0, 1, 0],
    [0, 0, 1],
  ],
};

const apply = (m, v) => m.map((row) => row[0] * v[0] + row[1] * v[1] + row[2] * v[2]);

export function simulateDichromacy(hex, kind = 'deuteranopia') {
  const projection = PROJECTIONS[kind];
  if (!projection) throw new Error(`unsupported dichromacy: ${kind}`);
  const lms = apply(RGB_TO_LMS, hexToRgb(hex).map(srgbToLinear));
  return rgbToHex(apply(LMS_TO_RGB, apply(projection, lms)).map(linearToSrgb));
}

/** CIEDE2000 between two colours as a deuteranope perceives them. */
export function deuteranopicDelta(hex1, hex2) {
  return ciede2000(simulateDichromacy(hex1, 'deuteranopia'), simulateDichromacy(hex2, 'deuteranopia'));
}

/** CIEDE2000 between two colours as a protanope perceives them. */
export function protanopicDelta(hex1, hex2) {
  return ciede2000(simulateDichromacy(hex1, 'protanopia'), simulateDichromacy(hex2, 'protanopia'));
}
