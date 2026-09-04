import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'liquid-highlight',
  title: 'Liquid Highlight',
  composition: Object.freeze({ slot: 'foreground', blend: 'alpha', zIndex: 45, priority: 45, exclusiveGroup: 'material-surface' }),
  uniforms: Object.freeze({
    uElementRect: Object.freeze({ type: 'vec4', default: [0.25, 0.25, 0.5, 0.5], description: 'Normalized [left, bottom, width, height] target rect.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.55, 0.92, 1], description: 'Liquid highlight tint.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.3, min: 0, max: 0.8, step: 0.01, description: 'Highlight opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.5, min: -2, max: 2, step: 0.01, description: 'Fixed-clock wave speed.' }),
    uWaveScale: Object.freeze({ type: 'float', default: 10, min: 2, max: 36, step: 0.25, description: 'Horizontal wave frequency.' }),
    uThickness: Object.freeze({ type: 'float', default: 0.035, min: 0.005, max: 0.16, step: 0.005, description: 'Ridge thickness.' }),
    uSoftness: Object.freeze({ type: 'float', default: 0.045, min: 0.005, max: 0.2, step: 0.005, description: 'Ridge feather.' }),
    uOffset: Object.freeze({ type: 'float', default: 0, min: -0.35, max: 0.2, step: 0.01, description: 'Vertical ridge offset.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ waveDetail: 1, alphaCap: 1 }),
    medium: Object.freeze({ waveDetail: 0.72, alphaCap: 0.78 }),
    low: Object.freeze({ waveDetail: 0.42, alphaCap: 0.52 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 0.42, medium: 0.27, low: 0.11 }),
    gpuMilliseconds: Object.freeze({ high: 0.12, medium: 0.075, low: 0.04 }),
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
    rect: gl.getUniformLocation(binding.program, 'uElementRect'), tint: gl.getUniformLocation(binding.program, 'uTint'), intensity: gl.getUniformLocation(binding.program, 'uIntensity'), speed: gl.getUniformLocation(binding.program, 'uSpeed'),
    waveScale: gl.getUniformLocation(binding.program, 'uWaveScale'), thickness: gl.getUniformLocation(binding.program, 'uThickness'), softness: gl.getUniformLocation(binding.program, 'uSoftness'), offset: gl.getUniformLocation(binding.program, 'uOffset'), waveDetail: gl.getUniformLocation(binding.program, 'uWaveDetail'), alphaCap: gl.getUniformLocation(binding.program, 'uAlphaCap'),
  });
  const definitions = manifest.uniforms;
  let targetRect = rect(initialParameters.uElementRect, definitions.uElementRect.default); let currentRect = targetRect.slice();
  let targetTint = tint(initialParameters.uTint, definitions.uTint.default); let currentTint = targetTint.slice();
  let targetIntensity = bounded(initialParameters.uIntensity, definitions.uIntensity); let currentIntensity = targetIntensity;
  let targetSpeed = bounded(initialParameters.uSpeed, definitions.uSpeed); let currentSpeed = targetSpeed;
  let targetWaveScale = bounded(initialParameters.uWaveScale, definitions.uWaveScale); let currentWaveScale = targetWaveScale;
  let targetThickness = bounded(initialParameters.uThickness, definitions.uThickness); let currentThickness = targetThickness;
  let targetSoftness = bounded(initialParameters.uSoftness, definitions.uSoftness); let currentSoftness = targetSoftness;
  let targetOffset = bounded(initialParameters.uOffset, definitions.uOffset); let currentOffset = targetOffset;
  let profile = manifest.quality.medium; let active = true; let disposed = false;
  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uElementRect')) targetRect = rect(next.uElementRect, targetRect); if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = tint(next.uTint, targetTint);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, definitions.uIntensity); if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = bounded(next.uSpeed, definitions.uSpeed);
    if (Object.prototype.hasOwnProperty.call(next, 'uWaveScale')) targetWaveScale = bounded(next.uWaveScale, definitions.uWaveScale); if (Object.prototype.hasOwnProperty.call(next, 'uThickness')) targetThickness = bounded(next.uThickness, definitions.uThickness);
    if (Object.prototype.hasOwnProperty.call(next, 'uSoftness')) targetSoftness = bounded(next.uSoftness, definitions.uSoftness); if (Object.prototype.hasOwnProperty.call(next, 'uOffset')) targetOffset = bounded(next.uOffset, definitions.uOffset);
  }
  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted); }
  function resize() {}
  function simulate(deltaTime) {
    for (let index = 0; index < 4; index += 1) currentRect[index] = ease(currentRect[index], targetRect[index], deltaTime); for (let index = 0; index < 3; index += 1) currentTint[index] = ease(currentTint[index], targetTint[index], deltaTime);
    currentIntensity = ease(currentIntensity, targetIntensity, deltaTime); currentSpeed = ease(currentSpeed, targetSpeed, deltaTime); currentWaveScale = ease(currentWaveScale, targetWaveScale, deltaTime); currentThickness = ease(currentThickness, targetThickness, deltaTime); currentSoftness = ease(currentSoftness, targetSoftness, deltaTime); currentOffset = ease(currentOffset, targetOffset, deltaTime);
  }
  function render(frame) {
    if (disposed || !active) return; gl.useProgram(binding.program); context.bindEngineGlobals(binding, frame);
    if (locations.rect !== null) gl.uniform4f(locations.rect, currentRect[0], currentRect[1], currentRect[2], currentRect[3]); if (locations.tint !== null) gl.uniform3f(locations.tint, currentTint[0], currentTint[1], currentTint[2]); if (locations.intensity !== null) gl.uniform1f(locations.intensity, currentIntensity); if (locations.speed !== null) gl.uniform1f(locations.speed, currentSpeed);
    if (locations.waveScale !== null) gl.uniform1f(locations.waveScale, currentWaveScale); if (locations.thickness !== null) gl.uniform1f(locations.thickness, currentThickness); if (locations.softness !== null) gl.uniform1f(locations.softness, currentSoftness); if (locations.offset !== null) gl.uniform1f(locations.offset, currentOffset); if (locations.waveDetail !== null) gl.uniform1f(locations.waveDetail, profile.waveDetail); if (locations.alphaCap !== null) gl.uniform1f(locations.alphaCap, profile.alphaCap);
    gl.bindVertexArray(context.fullscreenVao); gl.drawArrays(gl.TRIANGLES, 0, 3);
  }
  function dispose() { if (!disposed) { disposed = true; context.disposeProgram(binding); } }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const attribute = 'data-aurora-liquid-highlight-fallback'; const previous = root.getAttribute(attribute); root.setAttribute(attribute, detail.state || 'active'); let cleaned = false;
  return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); };
}
