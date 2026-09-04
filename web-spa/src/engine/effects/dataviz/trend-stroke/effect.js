import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'trend-stroke',
  title: 'Trend Stroke',
  composition: Object.freeze({slot: 'ambient',blend: 'additive',zIndex: 28,priority: 28,exclusiveGroup: 'dataviz-live'}),
  uniforms: Object.freeze({
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, step: 0.01, description: 'Draw-on progress owned by the host.' }),
    uThickness: Object.freeze({ type: 'float', default: 0.02, min: 0.004, max: 0.08, step: 0.002, description: 'Highlight thickness.' }),
    uAmplitude: Object.freeze({ type: 'float', default: 1, min: 0, max: 2, step: 0.01, description: 'Curve amplitude scale.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.38, min: 0, max: 0.9, step: 0.01, description: 'Highlight opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.75,alphaCap:0.82}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.55}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.3,medium:0.2,low:0.11}),
    gpuMilliseconds: Object.freeze({high:0.11,medium:0.07,low:0.04}),
    fill: 'element',
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

function smooth(current, target, deltaTime, rate) {
  const amount = 1 - Math.pow(0.001, Math.max(0, Math.min(deltaTime, 0.25)) * rate);
  return current + (target - current) * amount;
}

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const uProgressLocation = gl.getUniformLocation(binding.program, 'uProgress');
  const uThicknessLocation = gl.getUniformLocation(binding.program, 'uThickness');
  const uAmplitudeLocation = gl.getUniformLocation(binding.program, 'uAmplitude');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uProgressDefinition = manifest.uniforms.uProgress;
  const uThicknessDefinition = manifest.uniforms.uThickness;
  const uAmplitudeDefinition = manifest.uniforms.uAmplitude;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uProgress = bounded(initialParameters.uProgress, uProgressDefinition);
  let target_uThickness = bounded(initialParameters.uThickness, uThicknessDefinition);
  let target_uAmplitude = bounded(initialParameters.uAmplitude, uAmplitudeDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uProgress = target_uProgress;
  let uThickness = target_uThickness;
  let uAmplitude = target_uAmplitude;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uProgress')) target_uProgress = bounded(next.uProgress, uProgressDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uThickness')) target_uThickness = bounded(next.uThickness, uThicknessDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uAmplitude')) target_uAmplitude = bounded(next.uAmplitude, uAmplitudeDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) target_uIntensity = bounded(next.uIntensity, uIntensityDefinition);
  }

  function setQuality(quality, admitted) {
    profile = manifest.quality[quality] || manifest.quality.medium;
    active = Boolean(admitted);
  }

  function resize() {
    // Host owns the quality-capped resolution; nothing per-effect to recompute.
  }

  function simulate(deltaTime) {
    uProgress = smooth(uProgress, target_uProgress, deltaTime, 8);
    uThickness = smooth(uThickness, target_uThickness, deltaTime, 6);
    uAmplitude = smooth(uAmplitude, target_uAmplitude, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uProgressLocation !== null) gl.uniform1f(uProgressLocation, uProgress);
    if (uThicknessLocation !== null) gl.uniform1f(uThicknessLocation, uThickness);
    if (uAmplitudeLocation !== null) gl.uniform1f(uAmplitudeLocation, uAmplitude);
    if (uIntensityLocation !== null) gl.uniform1f(uIntensityLocation, uIntensity * profile.alphaCap);
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

export function applyDomFallback(root) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const attribute = 'data-trend-stroke-fallback';
  const previous = root.getAttribute(attribute);
  root.setAttribute(attribute, 'true');
  let cleaned = false;
  return () => {
    if (cleaned) return;
    cleaned = true;
    if (previous === null) root.removeAttribute(attribute);
    else root.setAttribute(attribute, previous);
  };
}
