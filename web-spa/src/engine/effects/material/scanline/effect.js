import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'scanline',
  title: 'Scanline',
  composition: Object.freeze({ slot: 'foreground', blend: 'alpha', zIndex: 41, priority: 41, exclusiveGroup: 'material-surface' }),
  uniforms: Object.freeze({
    uElementRect: Object.freeze({ type: 'vec4', default: [0.25, 0.25, 0.5, 0.5], description: 'Normalized [left, bottom, width, height] target rect.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.45, 0.82, 1], description: 'Scanline tint.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.2, min: 0, max: 0.6, step: 0.01, description: 'Overlay opacity.' }),
    uLineSpacing: Object.freeze({ type: 'float', default: 4, min: 1, max: 24, step: 0.25, description: 'Line spacing in CSS pixels before DPR scaling.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.24, min: -2, max: 2, step: 0.01, description: 'Fixed-clock vertical travel.' }),
    uThickness: Object.freeze({ type: 'float', default: 0.22, min: 0.03, max: 0.46, step: 0.01, description: 'Line duty cycle.' }),
    uSoftness: Object.freeze({ type: 'float', default: 0.08, min: 0.01, max: 0.3, step: 0.01, description: 'Line edge feather.' }),
    uSkew: Object.freeze({ type: 'float', default: 0.08, min: -0.8, max: 0.8, step: 0.01, description: 'Diagonal scanline skew.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ lineDensity: 1, contrast: 1, alphaCap: 1 }),
    medium: Object.freeze({ lineDensity: 0.78, contrast: 0.78, alphaCap: 0.76 }),
    low: Object.freeze({ lineDensity: 0.58, contrast: 0.56, alphaCap: 0.5 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 0.3, medium: 0.19, low: 0.08 }),
    gpuMilliseconds: Object.freeze({ high: 0.085, medium: 0.055, low: 0.03 }),
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

function ease(current, target, deltaTime) { return current + (target - current) * (1 - Math.pow(0.0001, Math.max(0, deltaTime))); }

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({
    rect: gl.getUniformLocation(binding.program, 'uElementRect'), tint: gl.getUniformLocation(binding.program, 'uTint'),
    intensity: gl.getUniformLocation(binding.program, 'uIntensity'), lineSpacing: gl.getUniformLocation(binding.program, 'uLineSpacing'),
    speed: gl.getUniformLocation(binding.program, 'uSpeed'), thickness: gl.getUniformLocation(binding.program, 'uThickness'),
    softness: gl.getUniformLocation(binding.program, 'uSoftness'), skew: gl.getUniformLocation(binding.program, 'uSkew'),
    lineDensity: gl.getUniformLocation(binding.program, 'uLineDensity'), contrast: gl.getUniformLocation(binding.program, 'uContrast'),
    alphaCap: gl.getUniformLocation(binding.program, 'uAlphaCap'),
  });
  const definitions = manifest.uniforms;
  let targetRect = rect(initialParameters.uElementRect, definitions.uElementRect.default); let currentRect = targetRect.slice();
  let targetTint = tint(initialParameters.uTint, definitions.uTint.default); let currentTint = targetTint.slice();
  let targetIntensity = bounded(initialParameters.uIntensity, definitions.uIntensity); let currentIntensity = targetIntensity;
  let targetLineSpacing = bounded(initialParameters.uLineSpacing, definitions.uLineSpacing); let currentLineSpacing = targetLineSpacing;
  let targetSpeed = bounded(initialParameters.uSpeed, definitions.uSpeed); let currentSpeed = targetSpeed;
  let targetThickness = bounded(initialParameters.uThickness, definitions.uThickness); let currentThickness = targetThickness;
  let targetSoftness = bounded(initialParameters.uSoftness, definitions.uSoftness); let currentSoftness = targetSoftness;
  let targetSkew = bounded(initialParameters.uSkew, definitions.uSkew); let currentSkew = targetSkew;
  let profile = manifest.quality.medium; let active = true; let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uElementRect')) targetRect = rect(next.uElementRect, targetRect);
    if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = tint(next.uTint, targetTint);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, definitions.uIntensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uLineSpacing')) targetLineSpacing = bounded(next.uLineSpacing, definitions.uLineSpacing);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = bounded(next.uSpeed, definitions.uSpeed);
    if (Object.prototype.hasOwnProperty.call(next, 'uThickness')) targetThickness = bounded(next.uThickness, definitions.uThickness);
    if (Object.prototype.hasOwnProperty.call(next, 'uSoftness')) targetSoftness = bounded(next.uSoftness, definitions.uSoftness);
    if (Object.prototype.hasOwnProperty.call(next, 'uSkew')) targetSkew = bounded(next.uSkew, definitions.uSkew);
  }
  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted); }
  function resize() {}
  function simulate(deltaTime) {
    for (let index = 0; index < 4; index += 1) currentRect[index] = ease(currentRect[index], targetRect[index], deltaTime);
    for (let index = 0; index < 3; index += 1) currentTint[index] = ease(currentTint[index], targetTint[index], deltaTime);
    currentIntensity = ease(currentIntensity, targetIntensity, deltaTime); currentLineSpacing = ease(currentLineSpacing, targetLineSpacing, deltaTime);
    currentSpeed = ease(currentSpeed, targetSpeed, deltaTime); currentThickness = ease(currentThickness, targetThickness, deltaTime);
    currentSoftness = ease(currentSoftness, targetSoftness, deltaTime); currentSkew = ease(currentSkew, targetSkew, deltaTime);
  }
  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program); context.bindEngineGlobals(binding, frame);
    if (locations.rect !== null) gl.uniform4f(locations.rect, currentRect[0], currentRect[1], currentRect[2], currentRect[3]);
    if (locations.tint !== null) gl.uniform3f(locations.tint, currentTint[0], currentTint[1], currentTint[2]);
    if (locations.intensity !== null) gl.uniform1f(locations.intensity, currentIntensity);
    if (locations.lineSpacing !== null) gl.uniform1f(locations.lineSpacing, currentLineSpacing);
    if (locations.speed !== null) gl.uniform1f(locations.speed, currentSpeed); if (locations.thickness !== null) gl.uniform1f(locations.thickness, currentThickness);
    if (locations.softness !== null) gl.uniform1f(locations.softness, currentSoftness); if (locations.skew !== null) gl.uniform1f(locations.skew, currentSkew);
    if (locations.lineDensity !== null) gl.uniform1f(locations.lineDensity, profile.lineDensity); if (locations.contrast !== null) gl.uniform1f(locations.contrast, profile.contrast);
    if (locations.alphaCap !== null) gl.uniform1f(locations.alphaCap, profile.alphaCap);
    gl.bindVertexArray(context.fullscreenVao); gl.drawArrays(gl.TRIANGLES, 0, 3);
  }
  function dispose() { if (!disposed) { disposed = true; context.disposeProgram(binding); } }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const attribute = 'data-aurora-scanline-fallback'; const previous = root.getAttribute(attribute); root.setAttribute(attribute, detail.state || 'active'); let cleaned = false;
  return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); };
}
