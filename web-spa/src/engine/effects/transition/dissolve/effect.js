import { clampUnit, ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

// The route coordinator owns the clock; this metadata deliberately uses only
// P2 motion tokens and lets callers interpolate uProgress on that timeline.
export const transition = Object.freeze({
  durationMs: 200,
  durationToken: '--pool-motion-normal',
  easing: 'cubic-bezier(.2, .8, .2, 1)',
  easingToken: '--pool-ease-standard',
  reducedMotion: 'skip-to-end',
});

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'dissolve',
  title: 'Dissolve transition',
  composition: Object.freeze({
    slot: 'interaction',
    blend: 'alpha',
    zIndex: 40,
    priority: 40,
    exclusiveGroup: 'route-transition',
  }),
  transition,
  uniforms: Object.freeze({
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, description: 'Transition progress from start to end.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.52, 0.77, 1], description: 'Dissolve edge color.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.34, min: 0, max: 0.72, description: 'Layer opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 1, min: 0, max: 2, description: 'Pattern drift only; does not change the 200ms duration.' }),
    uDensity: Object.freeze({ type: 'float', default: 0.58, min: 0.2, max: 1, description: 'Noise-cell density.' }),
    uScale: Object.freeze({ type: 'float', default: 1, min: 0.6, max: 1.8, description: 'Noise scale.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ noiseScale: 1, alphaCap: 1 }),
    medium: Object.freeze({ noiseScale: 0.78, alphaCap: 0.78 }),
    low: Object.freeze({ noiseScale: 0.54, alphaCap: 0.56 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 1.2, medium: 0.8, low: 0.4 }),
    gpuMilliseconds: Object.freeze({ high: 0.28, medium: 0.18, low: 0.1 }),
    fill: 'fullscreen',
    allocation: 'steady-state-zero-js',
    estimatedDrawCalls: 1,
    textureBytes: 0,
  }),
  threading: Object.freeze({ instructionGeneration: 'worker-safe', render: 'main-or-offscreen' }),
});

function bounded(value, definition) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric)) return definition.default;
  return Math.min(definition.max, Math.max(definition.min, numeric));
}

function readVec3(value, definition, target) {
  const source = Array.isArray(value) || ArrayBuffer.isView(value) ? value : definition.default;
  target[0] = clampUnit(Number(source[0]));
  target[1] = clampUnit(Number(source[1]));
  target[2] = clampUnit(Number(source[2]));
}

function approach(current, target, deltaSeconds) {
  const durationSeconds = transition.durationMs / 1000;
  const amount = 1 - Math.pow(0.001, Math.max(0, deltaSeconds) / durationSeconds);
  return current + (target - current) * amount;
}

function prefersReducedMotion() {
  return typeof globalThis.matchMedia === 'function'
    && globalThis.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({
    progress: gl.getUniformLocation(binding.program, 'uProgress'),
    tint: gl.getUniformLocation(binding.program, 'uTint'),
    intensity: gl.getUniformLocation(binding.program, 'uIntensity'),
    speed: gl.getUniformLocation(binding.program, 'uSpeed'),
    density: gl.getUniformLocation(binding.program, 'uDensity'),
    scale: gl.getUniformLocation(binding.program, 'uScale'),
  });
  const definitions = manifest.uniforms;
  let targetProgress = bounded(initialParameters.uProgress, definitions.uProgress);
  let currentProgress = targetProgress;
  let targetIntensity = bounded(initialParameters.uIntensity, definitions.uIntensity);
  let currentIntensity = targetIntensity;
  let targetSpeed = bounded(initialParameters.uSpeed, definitions.uSpeed);
  let currentSpeed = targetSpeed;
  let targetDensity = bounded(initialParameters.uDensity, definitions.uDensity);
  let currentDensity = targetDensity;
  let targetScale = bounded(initialParameters.uScale, definitions.uScale);
  let currentScale = targetScale;
  const targetTint = [0, 0, 0];
  const currentTint = [0, 0, 0];
  readVec3(initialParameters.uTint, definitions.uTint, targetTint);
  readVec3(initialParameters.uTint, definitions.uTint, currentTint);
  let profile = manifest.quality.medium;
  const reducedMotion = prefersReducedMotion();
  let active = !reducedMotion;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uProgress')) targetProgress = bounded(next.uProgress, definitions.uProgress);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, definitions.uIntensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = bounded(next.uSpeed, definitions.uSpeed);
    if (Object.prototype.hasOwnProperty.call(next, 'uDensity')) targetDensity = bounded(next.uDensity, definitions.uDensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uScale')) targetScale = bounded(next.uScale, definitions.uScale);
    if (Object.prototype.hasOwnProperty.call(next, 'uTint')) readVec3(next.uTint, definitions.uTint, targetTint);
  }

  function setQuality(quality, admitted) {
    profile = manifest.quality[quality] || manifest.quality.medium;
    active = Boolean(admitted) && !reducedMotion;
  }

  function resize() {}

  function simulate(deltaSeconds) {
    if (reducedMotion) {
      currentProgress = 1;
      return;
    }
    currentProgress = approach(currentProgress, targetProgress, deltaSeconds);
    currentIntensity = approach(currentIntensity, targetIntensity, deltaSeconds);
    currentSpeed = approach(currentSpeed, targetSpeed, deltaSeconds);
    currentDensity = approach(currentDensity, targetDensity, deltaSeconds);
    currentScale = approach(currentScale, targetScale, deltaSeconds);
    currentTint[0] = approach(currentTint[0], targetTint[0], deltaSeconds);
    currentTint[1] = approach(currentTint[1], targetTint[1], deltaSeconds);
    currentTint[2] = approach(currentTint[2], targetTint[2], deltaSeconds);
  }

  function render(frame) {
    if (disposed || !active || currentProgress <= 0.001 || currentProgress >= 0.999) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (locations.progress !== null) gl.uniform1f(locations.progress, clampUnit(currentProgress));
    if (locations.tint !== null) gl.uniform3fv(locations.tint, currentTint);
    if (locations.intensity !== null) gl.uniform1f(locations.intensity, currentIntensity * profile.alphaCap);
    if (locations.speed !== null) gl.uniform1f(locations.speed, currentSpeed);
    if (locations.density !== null) gl.uniform1f(locations.density, currentDensity * profile.noiseScale);
    if (locations.scale !== null) gl.uniform1f(locations.scale, currentScale);
    gl.bindVertexArray(context.fullscreenVao);
    gl.drawArrays(gl.TRIANGLES, 0, 3);
  }

  function dispose() {
    if (disposed) return;
    disposed = true;
    context.disposeProgram(binding);
  }

  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const key = 'data-aurora-dissolve-fallback';
  const reasonKey = 'data-aurora-dissolve-reason';
  const previous = root.getAttribute(key);
  const previousReason = root.getAttribute(reasonKey);
  root.setAttribute(key, 'true');
  root.setAttribute(reasonKey, String(detail.reason || 'transition-skipped'));
  return () => {
    if (previous === null) root.removeAttribute(key); else root.setAttribute(key, previous);
    if (previousReason === null) root.removeAttribute(reasonKey); else root.setAttribute(reasonKey, previousReason);
  };
}
