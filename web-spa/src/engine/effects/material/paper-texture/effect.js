import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'paper-texture',
  title: 'Paper Texture',
  composition: Object.freeze({ slot: 'foreground', blend: 'alpha', zIndex: 39, priority: 39, exclusiveGroup: 'material-surface' }),
  uniforms: Object.freeze({
    uElementRect: Object.freeze({ type: 'vec4', default: [0.25, 0.25, 0.5, 0.5], description: 'Normalized [left, bottom, width, height] target rect.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.88, 0.86, 0.8], description: 'Paper base tint.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.11, min: 0, max: 0.35, step: 0.01, description: 'Subtle surface opacity.' }),
    uFiberScale: Object.freeze({ type: 'float', default: 7, min: 1, max: 32, step: 0.25, description: 'Fiber cell size in CSS pixels before DPR scaling.' }),
    uFiberStrength: Object.freeze({ type: 'float', default: 0.12, min: 0, max: 0.35, step: 0.01, description: 'Fiber contrast.' }),
    uAge: Object.freeze({ type: 'float', default: 0.25, min: 0, max: 1, step: 0.01, description: 'Warm aged-paper bias.' }),
    uCornerRadius: Object.freeze({ type: 'float', default: 0.06, min: 0, max: 0.3, step: 0.01, description: 'Rounded paper mask.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ detail: 0.8, alphaCap: 1 }),
    medium: Object.freeze({ detail: 0.52, alphaCap: 0.72 }),
    low: Object.freeze({ detail: 0.2, alphaCap: 0.46 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 0.28, medium: 0.18, low: 0.07 }),
    gpuMilliseconds: Object.freeze({ high: 0.08, medium: 0.052, low: 0.028 }),
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
    rect: gl.getUniformLocation(binding.program, 'uElementRect'), tint: gl.getUniformLocation(binding.program, 'uTint'), intensity: gl.getUniformLocation(binding.program, 'uIntensity'), fiberScale: gl.getUniformLocation(binding.program, 'uFiberScale'), fiberStrength: gl.getUniformLocation(binding.program, 'uFiberStrength'), age: gl.getUniformLocation(binding.program, 'uAge'), cornerRadius: gl.getUniformLocation(binding.program, 'uCornerRadius'), detail: gl.getUniformLocation(binding.program, 'uDetail'), alphaCap: gl.getUniformLocation(binding.program, 'uAlphaCap'),
  });
  const definitions = manifest.uniforms;
  let targetRect = rect(initialParameters.uElementRect, definitions.uElementRect.default); let currentRect = targetRect.slice(); let targetTint = tint(initialParameters.uTint, definitions.uTint.default); let currentTint = targetTint.slice();
  let targetIntensity = bounded(initialParameters.uIntensity, definitions.uIntensity); let currentIntensity = targetIntensity; let targetFiberScale = bounded(initialParameters.uFiberScale, definitions.uFiberScale); let currentFiberScale = targetFiberScale; let targetFiberStrength = bounded(initialParameters.uFiberStrength, definitions.uFiberStrength); let currentFiberStrength = targetFiberStrength; let targetAge = bounded(initialParameters.uAge, definitions.uAge); let currentAge = targetAge; let targetCornerRadius = bounded(initialParameters.uCornerRadius, definitions.uCornerRadius); let currentCornerRadius = targetCornerRadius;
  let profile = manifest.quality.medium; let active = true; let disposed = false;
  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uElementRect')) targetRect = rect(next.uElementRect, targetRect); if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = tint(next.uTint, targetTint); if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, definitions.uIntensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uFiberScale')) targetFiberScale = bounded(next.uFiberScale, definitions.uFiberScale); if (Object.prototype.hasOwnProperty.call(next, 'uFiberStrength')) targetFiberStrength = bounded(next.uFiberStrength, definitions.uFiberStrength); if (Object.prototype.hasOwnProperty.call(next, 'uAge')) targetAge = bounded(next.uAge, definitions.uAge); if (Object.prototype.hasOwnProperty.call(next, 'uCornerRadius')) targetCornerRadius = bounded(next.uCornerRadius, definitions.uCornerRadius);
  }
  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted); } function resize() {}
  function simulate(deltaTime) { for (let index = 0; index < 4; index += 1) currentRect[index] = ease(currentRect[index], targetRect[index], deltaTime); for (let index = 0; index < 3; index += 1) currentTint[index] = ease(currentTint[index], targetTint[index], deltaTime); currentIntensity = ease(currentIntensity, targetIntensity, deltaTime); currentFiberScale = ease(currentFiberScale, targetFiberScale, deltaTime); currentFiberStrength = ease(currentFiberStrength, targetFiberStrength, deltaTime); currentAge = ease(currentAge, targetAge, deltaTime); currentCornerRadius = ease(currentCornerRadius, targetCornerRadius, deltaTime); }
  function render(frame) {
    if (disposed || !active) return; gl.useProgram(binding.program); context.bindEngineGlobals(binding, frame); if (locations.rect !== null) gl.uniform4f(locations.rect, currentRect[0], currentRect[1], currentRect[2], currentRect[3]); if (locations.tint !== null) gl.uniform3f(locations.tint, currentTint[0], currentTint[1], currentTint[2]); if (locations.intensity !== null) gl.uniform1f(locations.intensity, currentIntensity); if (locations.fiberScale !== null) gl.uniform1f(locations.fiberScale, currentFiberScale); if (locations.fiberStrength !== null) gl.uniform1f(locations.fiberStrength, currentFiberStrength); if (locations.age !== null) gl.uniform1f(locations.age, currentAge); if (locations.cornerRadius !== null) gl.uniform1f(locations.cornerRadius, currentCornerRadius); if (locations.detail !== null) gl.uniform1f(locations.detail, profile.detail); if (locations.alphaCap !== null) gl.uniform1f(locations.alphaCap, profile.alphaCap); gl.bindVertexArray(context.fullscreenVao); gl.drawArrays(gl.TRIANGLES, 0, 3);
  }
  function dispose() { if (!disposed) { disposed = true; context.disposeProgram(binding); } }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const attribute = 'data-aurora-paper-texture-fallback'; const previous = root.getAttribute(attribute); root.setAttribute(attribute, detail.state || 'active'); let cleaned = false;
  return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); };
}
