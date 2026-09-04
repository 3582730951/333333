import { clampUnit, ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const transition = Object.freeze({
  durationMs: 300,
  durationToken: '--pool-motion-slow',
  easing: 'cubic-bezier(.2, 0, 0, 1)',
  easingToken: '--pool-ease-emphasized',
  reducedMotion: 'skip-to-end',
});

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'ripple',
  title: 'Ripple transition',
  composition: Object.freeze({
    slot: 'interaction',
    blend: 'additive',
    zIndex: 41,
    priority: 41,
    exclusiveGroup: 'route-transition',
  }),
  transition,
  uniforms: Object.freeze({
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, description: 'Transition progress from origin to settled state.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.42, 0.84, 0.94], description: 'Ripple light color.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.32, min: 0, max: 0.7, description: 'Ripple opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 1, min: 0, max: 2, description: 'Ring shimmer only; does not change the 300ms duration.' }),
    uDensity: Object.freeze({ type: 'float', default: 0.55, min: 0.2, max: 1, description: 'Concentric-ring density.' }),
    uScale: Object.freeze({ type: 'float', default: 1, min: 0.6, max: 1.8, description: 'Ripple radius scale.' }),
    uOrigin: Object.freeze({ type: 'vec2', default: [0.5, 0.5], description: 'Normalized triggering-point origin.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ ringCount: 1, alphaCap: 1 }),
    medium: Object.freeze({ ringCount: 0.76, alphaCap: 0.76 }),
    low: Object.freeze({ ringCount: 0.52, alphaCap: 0.54 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 1.1, medium: 0.75, low: 0.38 }),
    gpuMilliseconds: Object.freeze({ high: 0.24, medium: 0.16, low: 0.09 }),
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

function readVector(value, definition, target, dimensions) {
  const source = Array.isArray(value) || ArrayBuffer.isView(value) ? value : definition.default;
  for (let index = 0; index < dimensions; index += 1) target[index] = clampUnit(Number(source[index]));
}

function approach(current, target, deltaSeconds) {
  const amount = 1 - Math.pow(0.001, Math.max(0, deltaSeconds) / (transition.durationMs / 1000));
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
    progress: gl.getUniformLocation(binding.program, 'uProgress'), tint: gl.getUniformLocation(binding.program, 'uTint'),
    intensity: gl.getUniformLocation(binding.program, 'uIntensity'), speed: gl.getUniformLocation(binding.program, 'uSpeed'),
    density: gl.getUniformLocation(binding.program, 'uDensity'), scale: gl.getUniformLocation(binding.program, 'uScale'),
    origin: gl.getUniformLocation(binding.program, 'uOrigin'),
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
  const targetTint = [0, 0, 0]; const currentTint = [0, 0, 0];
  const targetOrigin = [0, 0]; const currentOrigin = [0, 0];
  readVector(initialParameters.uTint, definitions.uTint, targetTint, 3);
  readVector(initialParameters.uTint, definitions.uTint, currentTint, 3);
  readVector(initialParameters.uOrigin, definitions.uOrigin, targetOrigin, 2);
  readVector(initialParameters.uOrigin, definitions.uOrigin, currentOrigin, 2);
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
    if (Object.prototype.hasOwnProperty.call(next, 'uTint')) readVector(next.uTint, definitions.uTint, targetTint, 3);
    if (Object.prototype.hasOwnProperty.call(next, 'uOrigin')) readVector(next.uOrigin, definitions.uOrigin, targetOrigin, 2);
  }

  function setQuality(quality, admitted) {
    profile = manifest.quality[quality] || manifest.quality.medium;
    active = Boolean(admitted) && !reducedMotion;
  }

  function resize() {}

  function simulate(deltaSeconds) {
    if (reducedMotion) { currentProgress = 1; return; }
    currentProgress = approach(currentProgress, targetProgress, deltaSeconds);
    currentIntensity = approach(currentIntensity, targetIntensity, deltaSeconds);
    currentSpeed = approach(currentSpeed, targetSpeed, deltaSeconds);
    currentDensity = approach(currentDensity, targetDensity, deltaSeconds);
    currentScale = approach(currentScale, targetScale, deltaSeconds);
    for (let index = 0; index < 3; index += 1) currentTint[index] = approach(currentTint[index], targetTint[index], deltaSeconds);
    for (let index = 0; index < 2; index += 1) currentOrigin[index] = approach(currentOrigin[index], targetOrigin[index], deltaSeconds);
  }

  function render(frame) {
    if (disposed || !active || currentProgress <= 0.001 || currentProgress >= 0.999) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (locations.progress !== null) gl.uniform1f(locations.progress, clampUnit(currentProgress));
    if (locations.tint !== null) gl.uniform3fv(locations.tint, currentTint);
    if (locations.intensity !== null) gl.uniform1f(locations.intensity, currentIntensity * profile.alphaCap);
    if (locations.speed !== null) gl.uniform1f(locations.speed, currentSpeed);
    if (locations.density !== null) gl.uniform1f(locations.density, currentDensity * profile.ringCount);
    if (locations.scale !== null) gl.uniform1f(locations.scale, currentScale);
    if (locations.origin !== null) gl.uniform2fv(locations.origin, currentOrigin);
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
  const key = 'data-aurora-ripple-fallback';
  const reasonKey = 'data-aurora-ripple-reason';
  const previous = root.getAttribute(key);
  const previousReason = root.getAttribute(reasonKey);
  root.setAttribute(key, 'true');
  root.setAttribute(reasonKey, String(detail.reason || 'transition-skipped'));
  return () => {
    if (previous === null) root.removeAttribute(key); else root.setAttribute(key, previous);
    if (previousReason === null) root.removeAttribute(reasonKey); else root.setAttribute(reasonKey, previousReason);
  };
}
