import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'holographic',
  title: 'Holographic',
  composition: Object.freeze({ slot: 'foreground', blend: 'alpha', zIndex: 43, priority: 43, exclusiveGroup: 'material-surface' }),
  uniforms: Object.freeze({
    uElementRect: Object.freeze({ type: 'vec4', default: [0.25, 0.25, 0.5, 0.5], description: 'Normalized [left, bottom, width, height] target rect.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.6, 0.95, 1], description: 'Holographic base tint.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.24, min: 0, max: 0.7, step: 0.01, description: 'Iridescent overlay opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.42, min: -2, max: 2, step: 0.01, description: 'Fixed-clock color sweep speed.' }),
    uStripeScale: Object.freeze({ type: 'float', default: 18, min: 2, max: 64, step: 0.5, description: 'Holographic stripe size in CSS pixels before DPR scaling.' }),
    uIridescence: Object.freeze({ type: 'float', default: 0.72, min: 0, max: 2, step: 0.01, description: 'Palette phase separation.' }),
    uNoise: Object.freeze({ type: 'float', default: 0.08, min: 0, max: 0.3, step: 0.01, description: 'Sparse sparkle amount.' }),
    uCornerRadius: Object.freeze({ type: 'float', default: 0.1, min: 0, max: 0.3, step: 0.01, description: 'Rounded surface mask.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ detail: 1, alphaCap: 1 }),
    medium: Object.freeze({ detail: 0.72, alphaCap: 0.78 }),
    low: Object.freeze({ detail: 0.42, alphaCap: 0.5 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 0.5, medium: 0.32, low: 0.13 }),
    gpuMilliseconds: Object.freeze({ high: 0.14, medium: 0.09, low: 0.045 }),
    fill: 'partial',
    allocation: 'steady-state-zero-js',
    estimatedDrawCalls: 1,
  }),
  threading: Object.freeze({ instructionGeneration: 'worker-safe', render: 'main-or-offscreen' }),
});

function bounded(value, definition) { const numeric = Number(value); return Number.isFinite(numeric) ? Math.min(definition.max, Math.max(definition.min, numeric)) : definition.default; }
function component(value, index, fallback, minimum, maximum) { const numeric = Number(value && value[index]); return Number.isFinite(numeric) ? Math.min(maximum, Math.max(minimum, numeric)) : fallback[index]; }
function rect(value, fallback) { return [component(value, 0, fallback, -0.5, 1.5), component(value, 1, fallback, -0.5, 1.5), component(value, 2, fallback, 0.001, 2), component(value, 3, fallback, 0.001, 2)]; }
function tint(value, fallback) { return [component(value, 0, fallback, 0, 1), component(value, 1, fallback, 0, 1), component(value, 2, fallback, 0, 1)]; }
function ease(current, target, deltaTime) { return current + (target - current) * (1 - Math.pow(0.0001, Math.max(0, deltaTime))); }

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({
    rect: gl.getUniformLocation(binding.program, 'uElementRect'), tint: gl.getUniformLocation(binding.program, 'uTint'), intensity: gl.getUniformLocation(binding.program, 'uIntensity'),
    speed: gl.getUniformLocation(binding.program, 'uSpeed'), stripeScale: gl.getUniformLocation(binding.program, 'uStripeScale'), iridescence: gl.getUniformLocation(binding.program, 'uIridescence'),
    noise: gl.getUniformLocation(binding.program, 'uNoise'), cornerRadius: gl.getUniformLocation(binding.program, 'uCornerRadius'), detail: gl.getUniformLocation(binding.program, 'uDetail'), alphaCap: gl.getUniformLocation(binding.program, 'uAlphaCap'),
  });
  const definitions = manifest.uniforms;
  let targetRect = rect(initialParameters.uElementRect, definitions.uElementRect.default); let currentRect = targetRect.slice();
  let targetTint = tint(initialParameters.uTint, definitions.uTint.default); let currentTint = targetTint.slice();
  let targetIntensity = bounded(initialParameters.uIntensity, definitions.uIntensity); let currentIntensity = targetIntensity;
  let targetSpeed = bounded(initialParameters.uSpeed, definitions.uSpeed); let currentSpeed = targetSpeed;
  let targetStripeScale = bounded(initialParameters.uStripeScale, definitions.uStripeScale); let currentStripeScale = targetStripeScale;
  let targetIridescence = bounded(initialParameters.uIridescence, definitions.uIridescence); let currentIridescence = targetIridescence;
  let targetNoise = bounded(initialParameters.uNoise, definitions.uNoise); let currentNoise = targetNoise;
  let targetCornerRadius = bounded(initialParameters.uCornerRadius, definitions.uCornerRadius); let currentCornerRadius = targetCornerRadius;
  let profile = manifest.quality.medium; let active = true; let disposed = false;
  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uElementRect')) targetRect = rect(next.uElementRect, targetRect);
    if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = tint(next.uTint, targetTint);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, definitions.uIntensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = bounded(next.uSpeed, definitions.uSpeed);
    if (Object.prototype.hasOwnProperty.call(next, 'uStripeScale')) targetStripeScale = bounded(next.uStripeScale, definitions.uStripeScale);
    if (Object.prototype.hasOwnProperty.call(next, 'uIridescence')) targetIridescence = bounded(next.uIridescence, definitions.uIridescence);
    if (Object.prototype.hasOwnProperty.call(next, 'uNoise')) targetNoise = bounded(next.uNoise, definitions.uNoise);
    if (Object.prototype.hasOwnProperty.call(next, 'uCornerRadius')) targetCornerRadius = bounded(next.uCornerRadius, definitions.uCornerRadius);
  }
  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted); }
  function resize() {}
  function simulate(deltaTime) {
    for (let index = 0; index < 4; index += 1) currentRect[index] = ease(currentRect[index], targetRect[index], deltaTime);
    for (let index = 0; index < 3; index += 1) currentTint[index] = ease(currentTint[index], targetTint[index], deltaTime);
    currentIntensity = ease(currentIntensity, targetIntensity, deltaTime); currentSpeed = ease(currentSpeed, targetSpeed, deltaTime); currentStripeScale = ease(currentStripeScale, targetStripeScale, deltaTime);
    currentIridescence = ease(currentIridescence, targetIridescence, deltaTime); currentNoise = ease(currentNoise, targetNoise, deltaTime); currentCornerRadius = ease(currentCornerRadius, targetCornerRadius, deltaTime);
  }
  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program); context.bindEngineGlobals(binding, frame);
    if (locations.rect !== null) gl.uniform4f(locations.rect, currentRect[0], currentRect[1], currentRect[2], currentRect[3]); if (locations.tint !== null) gl.uniform3f(locations.tint, currentTint[0], currentTint[1], currentTint[2]);
    if (locations.intensity !== null) gl.uniform1f(locations.intensity, currentIntensity); if (locations.speed !== null) gl.uniform1f(locations.speed, currentSpeed); if (locations.stripeScale !== null) gl.uniform1f(locations.stripeScale, currentStripeScale);
    if (locations.iridescence !== null) gl.uniform1f(locations.iridescence, currentIridescence); if (locations.noise !== null) gl.uniform1f(locations.noise, currentNoise); if (locations.cornerRadius !== null) gl.uniform1f(locations.cornerRadius, currentCornerRadius);
    if (locations.detail !== null) gl.uniform1f(locations.detail, profile.detail); if (locations.alphaCap !== null) gl.uniform1f(locations.alphaCap, profile.alphaCap);
    gl.bindVertexArray(context.fullscreenVao); gl.drawArrays(gl.TRIANGLES, 0, 3);
  }
  function dispose() { if (!disposed) { disposed = true; context.disposeProgram(binding); } }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const attribute = 'data-aurora-holographic-fallback'; const previous = root.getAttribute(attribute); root.setAttribute(attribute, detail.state || 'active'); let cleaned = false;
  return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); };
}
