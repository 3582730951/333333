import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'data-pulse',
  title: 'Data Pulse',
  composition: Object.freeze({slot: 'ambient',blend: 'additive',zIndex: 26,priority: 26,exclusiveGroup: 'dataviz-live'}),
  uniforms: Object.freeze({
    uRate: Object.freeze({ type: 'float', default: 0.5, min: 0, max: 3, step: 0.01, description: 'Pulses per second, fed from the real sample arrival rate.' }),
    uWidth: Object.freeze({ type: 'float', default: 0.14, min: 0.03, max: 0.4, step: 0.01, description: 'Pulse width.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.36, min: 0, max: 0.9, step: 0.01, description: 'Pulse opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.75,alphaCap:0.82}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.55}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.22,medium:0.15,low:0.09}),
    gpuMilliseconds: Object.freeze({high:0.08,medium:0.05,low:0.03}),
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
  const uRateLocation = gl.getUniformLocation(binding.program, 'uRate');
  const uWidthLocation = gl.getUniformLocation(binding.program, 'uWidth');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uRateDefinition = manifest.uniforms.uRate;
  const uWidthDefinition = manifest.uniforms.uWidth;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uRate = bounded(initialParameters.uRate, uRateDefinition);
  let target_uWidth = bounded(initialParameters.uWidth, uWidthDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uRate = target_uRate;
  let uWidth = target_uWidth;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uRate')) target_uRate = bounded(next.uRate, uRateDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uWidth')) target_uWidth = bounded(next.uWidth, uWidthDefinition);
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
    uRate = smooth(uRate, target_uRate, deltaTime, 6);
    uWidth = smooth(uWidth, target_uWidth, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uRateLocation !== null) gl.uniform1f(uRateLocation, uRate);
    if (uWidthLocation !== null) gl.uniform1f(uWidthLocation, uWidth);
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
  const attribute = 'data-data-pulse-fallback';
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
