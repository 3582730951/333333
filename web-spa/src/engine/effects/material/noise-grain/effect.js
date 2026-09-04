import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'noise-grain',
  title: 'Noise Grain',
  composition: Object.freeze({ slot: 'foreground', blend: 'alpha', zIndex: 40, priority: 40, exclusiveGroup: 'material-surface' }),
  uniforms: Object.freeze({
    uElementRect: Object.freeze({ type: 'vec4', default: [0.25, 0.25, 0.5, 0.5], description: 'Normalized [left, bottom, width, height] target rect.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.8, 0.9, 1], description: 'Grain tint mixed with the Aurora near/far palette.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.16, min: 0, max: 0.5, step: 0.01, description: 'Surface grain opacity.' }),
    uDensity: Object.freeze({ type: 'float', default: 0.9, min: 0.1, max: 2, step: 0.01, description: 'Procedural grain cell density.' }),
    uGrainSize: Object.freeze({ type: 'float', default: 1.2, min: 0.25, max: 4, step: 0.05, description: 'Grain size in CSS pixels before DPR scaling.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.2, min: 0, max: 2, step: 0.01, description: 'Fixed-clock grain evolution speed.' }),
    uCornerRadius: Object.freeze({ type: 'float', default: 0.08, min: 0, max: 0.3, step: 0.01, description: 'Rounded surface mask.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ grainDetail: 0.75, alphaCap: 1 }),
    medium: Object.freeze({ grainDetail: 0.48, alphaCap: 0.74 }),
    low: Object.freeze({ grainDetail: 0.2, alphaCap: 0.48 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 0.35, medium: 0.22, low: 0.09 }),
    gpuMilliseconds: Object.freeze({ high: 0.1, medium: 0.065, low: 0.035 }),
    fill: 'partial',
    allocation: 'steady-state-zero-js',
    estimatedDrawCalls: 1,
  }),
  threading: Object.freeze({ instructionGeneration: 'worker-safe', render: 'main-or-offscreen' }),
});

function bounded(value, definition) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric)) return definition.default;
  return Math.min(definition.max, Math.max(definition.min, numeric));
}

function component(value, index, fallback, minimum, maximum) {
  const numeric = Number(value && value[index]);
  return Number.isFinite(numeric) ? Math.min(maximum, Math.max(minimum, numeric)) : fallback[index];
}

function rect(value, fallback) {
  return [component(value, 0, fallback, -0.5, 1.5), component(value, 1, fallback, -0.5, 1.5), component(value, 2, fallback, 0.001, 2), component(value, 3, fallback, 0.001, 2)];
}

function tint(value, fallback) {
  return [component(value, 0, fallback, 0, 1), component(value, 1, fallback, 0, 1), component(value, 2, fallback, 0, 1)];
}

function ease(current, target, deltaTime) {
  return current + (target - current) * (1 - Math.pow(0.0001, Math.max(0, deltaTime)));
}

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({
    rect: gl.getUniformLocation(binding.program, 'uElementRect'), tint: gl.getUniformLocation(binding.program, 'uTint'),
    intensity: gl.getUniformLocation(binding.program, 'uIntensity'), density: gl.getUniformLocation(binding.program, 'uDensity'),
    grainSize: gl.getUniformLocation(binding.program, 'uGrainSize'), speed: gl.getUniformLocation(binding.program, 'uSpeed'),
    cornerRadius: gl.getUniformLocation(binding.program, 'uCornerRadius'), grainDetail: gl.getUniformLocation(binding.program, 'uGrainDetail'),
    alphaCap: gl.getUniformLocation(binding.program, 'uAlphaCap'),
  });
  const definitions = manifest.uniforms;
  let targetRect = rect(initialParameters.uElementRect, definitions.uElementRect.default);
  let currentRect = targetRect.slice();
  let targetTint = tint(initialParameters.uTint, definitions.uTint.default);
  let currentTint = targetTint.slice();
  let targetIntensity = bounded(initialParameters.uIntensity, definitions.uIntensity);
  let currentIntensity = targetIntensity;
  let targetDensity = bounded(initialParameters.uDensity, definitions.uDensity);
  let currentDensity = targetDensity;
  let targetGrainSize = bounded(initialParameters.uGrainSize, definitions.uGrainSize);
  let currentGrainSize = targetGrainSize;
  let targetSpeed = bounded(initialParameters.uSpeed, definitions.uSpeed);
  let currentSpeed = targetSpeed;
  let targetCornerRadius = bounded(initialParameters.uCornerRadius, definitions.uCornerRadius);
  let currentCornerRadius = targetCornerRadius;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uElementRect')) targetRect = rect(next.uElementRect, targetRect);
    if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = tint(next.uTint, targetTint);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, definitions.uIntensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uDensity')) targetDensity = bounded(next.uDensity, definitions.uDensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uGrainSize')) targetGrainSize = bounded(next.uGrainSize, definitions.uGrainSize);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = bounded(next.uSpeed, definitions.uSpeed);
    if (Object.prototype.hasOwnProperty.call(next, 'uCornerRadius')) targetCornerRadius = bounded(next.uCornerRadius, definitions.uCornerRadius);
  }

  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted); }
  function resize() {}

  function simulate(deltaTime) {
    for (let index = 0; index < 4; index += 1) currentRect[index] = ease(currentRect[index], targetRect[index], deltaTime);
    for (let index = 0; index < 3; index += 1) currentTint[index] = ease(currentTint[index], targetTint[index], deltaTime);
    currentIntensity = ease(currentIntensity, targetIntensity, deltaTime);
    currentDensity = ease(currentDensity, targetDensity, deltaTime);
    currentGrainSize = ease(currentGrainSize, targetGrainSize, deltaTime);
    currentSpeed = ease(currentSpeed, targetSpeed, deltaTime);
    currentCornerRadius = ease(currentCornerRadius, targetCornerRadius, deltaTime);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (locations.rect !== null) gl.uniform4f(locations.rect, ...currentRect);
    if (locations.tint !== null) gl.uniform3f(locations.tint, ...currentTint);
    if (locations.intensity !== null) gl.uniform1f(locations.intensity, currentIntensity);
    if (locations.density !== null) gl.uniform1f(locations.density, currentDensity);
    if (locations.grainSize !== null) gl.uniform1f(locations.grainSize, currentGrainSize);
    if (locations.speed !== null) gl.uniform1f(locations.speed, currentSpeed);
    if (locations.cornerRadius !== null) gl.uniform1f(locations.cornerRadius, currentCornerRadius);
    if (locations.grainDetail !== null) gl.uniform1f(locations.grainDetail, profile.grainDetail);
    if (locations.alphaCap !== null) gl.uniform1f(locations.alphaCap, profile.alphaCap);
    gl.bindVertexArray(context.fullscreenVao);
    gl.drawArrays(gl.TRIANGLES, 0, 3);
  }

  function dispose() { if (!disposed) { disposed = true; context.disposeProgram(binding); } }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const attribute = 'data-aurora-noise-grain-fallback';
  const previous = root.getAttribute(attribute);
  root.setAttribute(attribute, detail.state || 'active');
  let cleaned = false;
  return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); };
}
