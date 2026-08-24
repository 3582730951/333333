// Hand-written WebGL2 atmosphere: the console's 3D-first immersion layer.
//
// Why not a 3D library. The initial JavaScript budget is 256 KiB gzip and
// `scripts/check-build-budget.mjs` throws when it is exceeded; three.js alone is
// roughly two thirds of that before anything is drawn with it. What this layer
// actually needs is one fullscreen triangle and one fragment shader -- a scene
// graph, a camera rig and a loader stack would all be dead weight around it. So
// this is raw WebGL2 with no dependency, and it lazy-loads besides.
//
// Why it takes its colours as arguments. `check:pool-ui-migration` rejects colour
// literals in every source file outside styles/tokens.css. The caller reads the
// palette off the document element and hands it in, which is also what makes the
// field follow the active theme instead of carrying a second, silently drifting
// copy of the brand.
//
// Contract: pure module, no React, no globals beyond the canvas it is handed and
// the animation-frame helpers. `dispose()` releases every GL object it created, so
// a hot-module replacement cycle cannot leak a context.

import { requestBrowserAnimationFrame, cancelBrowserAnimationFrame } from './browserLifecycle.js';

// How long the field keeps drifting after the last thing that gave it something to
// say, before it parks.
//
// A background that animates forever is worse on every axis than one that settles.
// It costs a composited frame every 16ms for motion nobody is watching; it drains a
// laptop for a page left open on a second monitor; and -- the concrete failure that
// produced this constant -- a frame that never goes quiescent never fires Chrome's
// `networkIdle` lifecycle event, so every tool that waits on network idle hangs on
// the console forever. That includes this repo's own visual-smoke gate, and it would
// equally include any CI screenshotter, crawler, or uptime check pointed at it.
//
// So the loop is demand-driven: it wakes on anything with a reason to move -- the
// pointer, a new live sample, a theme change, a resize -- and parks a few seconds
// later. The drift is slow enough that a parked frame is indistinguishable from a
// moving one until you move the mouse, at which point it is running again.
const IDLE_AFTER_MS = 4_000;
// Eased values within this of their target are treated as arrived, so the loop is
// not held open forever by an asymptote it never mathematically reaches.
const SETTLED_EPSILON = 0.002;

export const VERTEX_SOURCE = `#version 300 es
// Fullscreen triangle, no vertex buffer: gl_VertexID drives the three corners.
// Cheaper than a quad (no diagonal seam, three vertices instead of six) and it
// needs no attribute state to be bound or torn down.
out vec2 vUv;
void main() {
  vec2 corner = vec2((gl_VertexID << 1) & 2, gl_VertexID & 2);
  vUv = corner;
  gl_Position = vec4(corner * 2.0 - 1.0, 0.0, 1.0);
}
`;

export const FRAGMENT_SOURCE = `#version 300 es
precision mediump float;

// Must mirror the vertex stage's out-declaration exactly. GLSL ES 3.00 has no
// implicit varyings: omitting this line compiles the vertex shader fine and fails
// the fragment shader on first use, which reads as "WebGL unavailable" rather than
// as a typo. tests/atmosphere-shader.test.ts pins the two declarations together.
in vec2 vUv;

uniform vec2 uResolution;
uniform float uTime;
uniform vec2 uPointer;
uniform float uEnergy;
uniform float uAlpha;
uniform float uGrain;
uniform vec3 uVoid;
uniform vec3 uNear;
uniform vec3 uFar;
uniform vec3 uGlow;

out vec4 fragColor;

float hash21(vec2 p) {
  p = fract(p * vec2(123.34, 456.21));
  p += dot(p, p + 45.32);
  return fract(p.x * p.y);
}

float vnoise(vec2 p) {
  vec2 i = floor(p);
  vec2 f = fract(p);
  vec2 u = f * f * (3.0 - 2.0 * f);
  float a = hash21(i);
  float b = hash21(i + vec2(1.0, 0.0));
  float c = hash21(i + vec2(0.0, 1.0));
  float d = hash21(i + vec2(1.0, 1.0));
  return mix(mix(a, b, u.x), mix(c, d, u.x), u.y);
}

// Four octaves is where the silhouette stops changing at this scale; a fifth costs
// a full-screen texture fetch pass worth of ALU and is invisible under the blur.
float fbm(vec2 p) {
  float value = 0.0;
  float amplitude = 0.5;
  for (int i = 0; i < 4; i++) {
    value += amplitude * vnoise(p);
    p *= 2.03;
    amplitude *= 0.5;
  }
  return value;
}

// A soft light well. Falloff is exp rather than smoothstep so neighbouring wells
// blend into one continuous field instead of showing their own edges -- that edge
// is what makes a CSS radial-gradient stack read as flat discs.
float well(vec2 uv, vec2 centre, float radius) {
  float d = length((uv - centre) / vec2(1.0, 0.72));
  return exp(-d * d / max(radius, 0.0001));
}

void main() {
  vec2 uv = vUv;
  float aspect = uResolution.x / max(uResolution.y, 1.0);
  vec2 p = vec2((uv.x - 0.5) * aspect, uv.y - 0.5);

  // Domain warp. The field is displaced by its own noise before it is sampled,
  // which is what gives the boundaries their organic, hand-mixed edge.
  float t = uTime * 0.026;
  vec2 warp = vec2(
    fbm(p * 1.6 + vec2(t, -t * 0.7)),
    fbm(p * 1.6 + vec2(4.7 - t * 0.8, 2.1 + t))
  );
  vec2 q = p + (warp - 0.5) * (0.42 + uEnergy * 0.22);

  // Three wells at three depths, evaluated once per colour channel at slightly
  // different offsets.
  //
  // That per-channel offset is chromatic aberration -- the RGB fringing a real lens
  // leaves toward the edge of frame, and the single cheapest thing that stops a
  // procedural gradient reading as "a CSS gradient with extra steps". The warp and
  // the noise are computed once and shared; only the well evaluations repeat, which
  // is a handful of exp() calls rather than another fbm.
  //
  // Strength scales with radius because that is where a lens actually disperses,
  // and it stays off in the middle third of the screen where the content sits.
  vec2 parallax = uPointer * 0.055;
  vec2 nearC = vec2(-0.34, 0.20) + parallax * 1.7 + vec2(sin(t * 1.7) * 0.05, cos(t * 1.3) * 0.04);
  vec2 farC  = vec2( 0.38, -0.16) + parallax * 0.9 + vec2(cos(t * 1.1) * 0.06, sin(t * 1.6) * 0.05);
  vec2 glowC = vec2( 0.02, -0.40) + parallax * 0.4 + vec2(sin(t * 0.9) * 0.08, 0.0);

  float radial = smoothstep(0.28, 1.05, length(p));
  vec2 disperse = normalize(p + vec2(1e-5)) * radial * 0.018;

  vec3 colour = uVoid;
  vec3 nearRGB = vec3(0.0);
  vec3 farRGB = vec3(0.0);
  vec3 glowRGB = vec3(0.0);
  for (int i = 0; i < 3; i++) {
    // -1, 0, +1 for R, G, B: green stays put and the outer channels split around it.
    vec2 offset = disperse * (float(i) - 1.0);
    vec2 qc = q + offset;
    nearRGB[i] = clamp(well(qc, nearC, 0.34), 0.0, 1.0);
    farRGB[i]  = clamp(well(qc, farC,  0.46), 0.0, 1.0);
    glowRGB[i] = clamp(well(qc, glowC, 0.30), 0.0, 1.0);
  }

  // Weights, not replacements. These were 0.85 / 0.80 / 0.10+0.42E, which drove the
  // wells almost all the way to their own colour and turned every dark page into a
  // saturated green wash -- the field became the subject instead of setting a mood
  // under one. The console is an instrument; the background's job is to give it
  // somewhere to sit.
  colour = mix(colour, uFar,  farRGB  * 0.50);
  colour = mix(colour, uNear, nearRGB * 0.44);

  // The accent is the only saturated thing on screen and it stays scarce: its
  // weight is driven by live pool energy, so the field brightens under load and
  // settles when the pool is idle. That is the whole "show, don't tell" of it.
  vec3 accent = glowRGB * (0.04 + uEnergy * 0.20);
  colour = mix(colour, uGlow, accent);

  // Depth ramp plus vignette. Both exist to protect legibility: content sits on
  // this layer, so the corners and the top must fall back toward the void.
  float depth = smoothstep(-0.55, 0.55, q.y * 0.6 + fbm(q * 0.9 + t) * 0.25);
  colour = mix(colour, uVoid, (1.0 - depth) * 0.38);
  // Tightened from smoothstep(1.25, 0.28, ...): the old falloff only pulled back at
  // the extreme corners, so the field reached every edge of every page. Now it
  // concentrates and most of the screen is within a few percent of the void.
  float vignette = smoothstep(1.02, 0.16, length(p * vec2(0.82, 1.05)));
  colour = mix(uVoid, colour, vignette);

  // A measured dot lattice, not a texture.
  //
  // Photographic grain is the wrong idiom for an instrument: it says "film", and
  // this surface is a control panel. A regular lattice of sub-pixel dots says
  // "measured" instead -- the same register as a blueprint or an oscilloscope
  // graticule -- and it is what the reference gallery uses under its loader. It
  // rides on top of the field at a fixed device-pixel pitch so it never scales
  // with the render buffer, and it fades out where the field is brightest so it
  // never sits on top of the accent.
  vec2 cell = fract(gl_FragCoord.xy / 6.0) - 0.5;
  float lattice = 1.0 - smoothstep(0.0, 0.34, length(cell));
  colour += lattice * uGrain * 0.55 * (1.0 - vignette * 0.35);

  // Animated noise underneath it, at a fraction of the amplitude. Its only job now
  // is to break the banding an 8-bit gradient this large would otherwise show; the
  // lattice above carries the texture.
  float grain = hash21(gl_FragCoord.xy + fract(uTime) * 91.7) - 0.5;
  colour += grain * uGrain * 0.5;

  fragColor = vec4(clamp(colour, 0.0, 1.0), uAlpha);
}
`;

function compile(gl, type, source, onDiagnostic) {
  const shader = gl.createShader(type);
  if (!shader) return null;
  gl.shaderSource(shader, source);
  gl.compileShader(shader);
  if (!gl.getShaderParameter(shader, gl.COMPILE_STATUS)) {
    onDiagnostic(`shader compile failed: ${gl.getShaderInfoLog(shader)}`);
    gl.deleteShader(shader);
    return null;
  }
  return shader;
}

function link(gl, onDiagnostic) {
  const vertex = compile(gl, gl.VERTEX_SHADER, VERTEX_SOURCE, onDiagnostic);
  const fragment = compile(gl, gl.FRAGMENT_SHADER, FRAGMENT_SOURCE, onDiagnostic);
  if (!vertex || !fragment) {
    if (vertex) gl.deleteShader(vertex);
    if (fragment) gl.deleteShader(fragment);
    return null;
  }
  const program = gl.createProgram();
  if (!program) {
    gl.deleteShader(vertex);
    gl.deleteShader(fragment);
    return null;
  }
  gl.attachShader(program, vertex);
  gl.attachShader(program, fragment);
  gl.linkProgram(program);
  // Shaders are detached immediately: the linked program holds its own copy, and
  // leaving them attached keeps the sources alive for the life of the program.
  gl.detachShader(program, vertex);
  gl.detachShader(program, fragment);
  gl.deleteShader(vertex);
  gl.deleteShader(fragment);
  if (!gl.getProgramParameter(program, gl.LINK_STATUS)) {
    onDiagnostic(`program link failed: ${gl.getProgramInfoLog(program)}`);
    gl.deleteProgram(program);
    return null;
  }
  return program;
}

/**
 * Parses a token value into linear-ish 0-1 components.
 *
 * Accepts the two forms tokens.css actually produces once resolved by the browser:
 * a six-digit hex, or a space/comma separated colour function body. Returns null
 * when it cannot parse, and the caller keeps the previous uniform rather than
 * flashing an unintended colour.
 */
export function parseColorChannels(value) {
  const text = String(value || '').trim();
  if (!text) return null;
  if (text.charCodeAt(0) === 35) {
    const body = text.slice(1);
    const full = body.length === 3 ? body.split('').map((c) => c + c).join('') : body;
    if (!/^[0-9a-f]{6}$/i.test(full)) return null;
    return [0, 2, 4].map((i) => parseInt(full.slice(i, i + 2), 16) / 255);
  }
  const numbers = text.match(/-?\d*\.?\d+/g);
  if (!numbers || numbers.length < 3) return null;
  return numbers.slice(0, 3).map((n) => Math.min(1, Math.max(0, Number(n) / 255)));
}

export const UNIFORM_NAMES = [
  'uResolution', 'uTime', 'uPointer', 'uEnergy', 'uAlpha', 'uGrain',
  'uVoid', 'uNear', 'uFar', 'uGlow',
];

/**
 * Creates the atmosphere renderer over an existing canvas.
 *
 * Returns null when WebGL2 is unavailable or the program fails to build, which is
 * the caller's signal to leave the CSS fallback in place. Never throws.
 *
 * @param {HTMLCanvasElement | null} canvas
 * @param {{ onDiagnostic?: (message: string) => void }} [options]
 */
export function createAtmosphere(canvas, { onDiagnostic = () => {} } = {}) {
  if (!canvas || typeof canvas.getContext !== 'function') return null;
  // Asked before getContext so a jsdom test run does not trip its "not implemented"
  // warning on every mount, and so a browser without WebGL2 costs no context probe.
  if (typeof WebGL2RenderingContext === 'undefined') return null;
  let gl = null;
  try {
    gl = canvas.getContext('webgl2', {
      alpha: true,
      antialias: false,
      depth: false,
      stencil: false,
      // The layer is painted once per frame and never read back, so the browser is
      // free to discard it after compositing.
      preserveDrawingBuffer: false,
      powerPreference: 'low-power',
    });
  } catch (error) {
    onDiagnostic(`webgl2 context refused: ${error}`);
    gl = null;
  }
  // No WebGL2 at all is an ordinary machine capability, not a fault, so it is not
  // reported. A context that exists but will not build a program is a real defect.
  if (!gl) return null;

  const program = link(gl, onDiagnostic);
  if (!program) return null;

  const vao = gl.createVertexArray();
  const uniforms = {};
  for (const name of UNIFORM_NAMES) uniforms[name] = gl.getUniformLocation(program, name);

  const state = {
    width: 0,
    height: 0,
    scale: 1,
    time: 0,
    energy: 0,
    targetEnergy: 0,
    pointer: [0, 0],
    targetPointer: [0, 0],
    alpha: 1,
    grain: 0.05,
    colours: { uVoid: [0, 0, 0], uNear: [0, 0, 0], uFar: [0, 0, 0], uGlow: [0, 0, 0] },
  };

  let frame = null;
  let running = false;
  let disposed = false;
  let lastStamp = 0;
  let activeUntil = 0;
  let clock = 0;

  // Anything that changes what should be on screen extends the active window and
  // restarts the loop if it had parked.
  const wake = () => {
    if (disposed) return;
    activeUntil = clock + IDLE_AFTER_MS;
    if (!running) start();
  };

  const settled = () => (
    Math.abs(state.targetEnergy - state.energy) < SETTLED_EPSILON
    && Math.abs(state.targetPointer[0] - state.pointer[0]) < SETTLED_EPSILON
    && Math.abs(state.targetPointer[1] - state.pointer[1]) < SETTLED_EPSILON
  );

  const resize = (cssWidth, cssHeight, pixelRatio) => {
    if (disposed) return;
    // Rendering below device resolution is invisible on a field this soft and is
    // the single biggest lever on fill cost -- this shader is entirely
    // fragment-bound. Capped so a 3x phone does not render a 3x background.
    const ratio = Math.min(Math.max(pixelRatio || 1, 1), 2) * 0.62;
    const width = Math.max(1, Math.round(cssWidth * ratio));
    const height = Math.max(1, Math.round(cssHeight * ratio));
    if (width === state.width && height === state.height) return;
    state.width = width;
    state.height = height;
    canvas.width = width;
    canvas.height = height;
    gl.viewport(0, 0, width, height);
    // A resized buffer is blank until something draws into it.
    wake();
  };

  const setPalette = (palette) => {
    for (const key of ['uVoid', 'uNear', 'uFar', 'uGlow']) {
      const parsed = parseColorChannels(palette?.[key]);
      if (parsed) state.colours[key] = parsed;
    }
    if (Number.isFinite(palette?.alpha)) state.alpha = Math.min(1, Math.max(0, palette.alpha));
    if (Number.isFinite(palette?.grain)) state.grain = Math.min(0.4, Math.max(0, palette.grain));
    wake();
  };

  // Energy and pointer are targets, not values: both are eased toward on every
  // frame so a step change in live metrics arrives as a swell rather than a jump.
  const setEnergy = (value) => {
    if (!Number.isFinite(value)) return;
    const next = Math.min(1, Math.max(0, value));
    // An unchanged sample is most of them: the server only pushes a delta when
    // something moved, and waking for a repeat would defeat the parking entirely.
    if (next === state.targetEnergy) return;
    state.targetEnergy = next;
    wake();
  };
  const setPointer = (x, y) => {
    const next = [
      Math.min(1, Math.max(-1, Number(x) || 0)),
      Math.min(1, Math.max(-1, Number(y) || 0)),
    ];
    if (next[0] === state.targetPointer[0] && next[1] === state.targetPointer[1]) return;
    state.targetPointer = next;
    wake();
  };

  const draw = (stamp) => {
    if (disposed || !running) return;
    // A driver reset, a backgrounded GPU process, or a tab restore can take the
    // context away underneath the loop. Drawing into a lost context is silently a
    // no-op that keeps costing a frame every 16ms, so stop instead.
    if (gl.isContextLost()) {
      running = false;
      frame = null;
      return;
    }
    const delta = lastStamp ? Math.min(64, stamp - lastStamp) : 16;
    lastStamp = stamp;
    clock += delta;
    state.time += delta / 1000;

    const ease = 1 - Math.pow(0.0016, delta / 1000);
    state.energy += (state.targetEnergy - state.energy) * ease;
    state.pointer[0] += (state.targetPointer[0] - state.pointer[0]) * ease;
    state.pointer[1] += (state.targetPointer[1] - state.pointer[1]) * ease;

    gl.useProgram(program);
    gl.bindVertexArray(vao);
    gl.uniform2f(uniforms.uResolution, state.width, state.height);
    gl.uniform1f(uniforms.uTime, state.time);
    gl.uniform2f(uniforms.uPointer, state.pointer[0], state.pointer[1]);
    gl.uniform1f(uniforms.uEnergy, state.energy);
    gl.uniform1f(uniforms.uAlpha, state.alpha);
    gl.uniform1f(uniforms.uGrain, state.grain);
    gl.uniform3fv(uniforms.uVoid, state.colours.uVoid);
    gl.uniform3fv(uniforms.uNear, state.colours.uNear);
    gl.uniform3fv(uniforms.uFar, state.colours.uFar);
    gl.uniform3fv(uniforms.uGlow, state.colours.uGlow);
    gl.drawArrays(gl.TRIANGLES, 0, 3);

    // Park once the active window has elapsed and every eased value has arrived --
    // stopping mid-ease would freeze the field visibly part-way through a move.
    if (clock > activeUntil && settled()) {
      running = false;
      frame = null;
      return;
    }
    frame = requestBrowserAnimationFrame(draw);
  };

  function start() {
    if (disposed || running || gl.isContextLost()) return;
    running = true;
    lastStamp = 0;
    if (activeUntil <= clock) activeUntil = clock + IDLE_AFTER_MS;
    frame = requestBrowserAnimationFrame(draw);
  }

  const stop = () => {
    running = false;
    if (frame != null) cancelBrowserAnimationFrame(frame);
    frame = null;
  };

  /** Renders exactly one frame. Used for the reduced-motion still. */
  const renderStill = () => {
    if (disposed) return;
    state.energy = state.targetEnergy;
    state.pointer = [...state.targetPointer];
    running = true;
    draw(0);
    stop();
  };

  const dispose = () => {
    stop();
    disposed = true;
    try {
      gl.deleteVertexArray(vao);
      gl.deleteProgram(program);
    } catch {
      // A context already lost throws here and there is nothing left to release.
    }
    // Deliberately NOT calling WEBGL_lose_context.loseContext().
    //
    // It was here first, to drop the drawing buffer rather than wait for the canvas
    // to be collected. It is wrong, and quietly so. React reuses the same canvas
    // element across a StrictMode mount/cleanup/mount cycle, and getContext returns
    // the SAME context object for a given canvas -- so losing it on cleanup handed
    // the second mount a permanently dead context. The layer then rendered nothing
    // and the lost context composited as opaque white over the entire page, while
    // the shader itself measured perfectly correct in isolation.
    //
    // There is also nothing to defend against: a repeated mount on one canvas
    // creates one context, not many, and a genuinely unmounted canvas is removed
    // from the DOM and collected with its context.
  };

  return { resize, setPalette, setEnergy, setPointer, start, stop, renderStill, dispose };
}
