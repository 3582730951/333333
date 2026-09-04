import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'number-roll',
  title: 'Number Roll',
  composition: Object.freeze({slot: 'interaction',blend: 'additive',zIndex: 38,priority: 38,exclusiveGroup: 'numeric-feedback'}),
  uniforms: Object.freeze({
    uRoll: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, step: 0.001, description: 'Roll phase driven by the host value change.' }),
    uBlur: Object.freeze({ type: 'float', default: 0.18, min: 0.04, max: 0.5, step: 0.01, description: 'Vertical motion blur width.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.28, min: 0, max: 0.7, step: 0.01, description: 'Blur band opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.7,alphaCap:0.8}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.5}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.18,medium:0.12,low:0.07}),
    gpuMilliseconds: Object.freeze({high:0.07,medium:0.045,low:0.025}),
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
  const uRollLocation = gl.getUniformLocation(binding.program, 'uRoll');
  const uBlurLocation = gl.getUniformLocation(binding.program, 'uBlur');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uRollDefinition = manifest.uniforms.uRoll;
  const uBlurDefinition = manifest.uniforms.uBlur;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uRoll = bounded(initialParameters.uRoll, uRollDefinition);
  let target_uBlur = bounded(initialParameters.uBlur, uBlurDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uRoll = target_uRoll;
  let uBlur = target_uBlur;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uRoll')) target_uRoll = bounded(next.uRoll, uRollDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uBlur')) target_uBlur = bounded(next.uBlur, uBlurDefinition);
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
    uRoll = smooth(uRoll, target_uRoll, deltaTime, 12);
    uBlur = smooth(uBlur, target_uBlur, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uRollLocation !== null) gl.uniform1f(uRollLocation, uRoll);
    if (uBlurLocation !== null) gl.uniform1f(uBlurLocation, uBlur);
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
  const attribute = 'data-number-roll-fallback';
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
