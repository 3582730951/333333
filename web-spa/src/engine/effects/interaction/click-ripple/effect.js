import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'click-ripple',
  title: 'Click Ripple',
  composition: Object.freeze({slot: 'interaction',blend: 'additive',zIndex: 43,priority: 43,exclusiveGroup: 'pointer-press'}),
  uniforms: Object.freeze({
    uOrigin: Object.freeze({ type: 'vec2', default: [0.5,0.5], min: 0, max: 1, step: 0.001, description: 'Ripple origin in effect UV space.' }),
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, step: 0.01, description: 'Ripple envelope owned by the host.' }),
    uWidth: Object.freeze({ type: 'float', default: 0.09, min: 0.02, max: 0.3, step: 0.005, description: 'Ring thickness.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.45, min: 0, max: 1, step: 0.01, description: 'Ring opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.75,alphaCap:0.85}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.6}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.24,medium:0.16,low:0.09}),
    gpuMilliseconds: Object.freeze({high:0.09,medium:0.06,low:0.03}),
    fill: 'element',
    allocation: 'steady-state-zero-js',
    estimatedDrawCalls: 1,
    textureBytes: 0,
  }),
  threading: Object.freeze({ instructionGeneration: 'main-only', render: 'main-or-offscreen' }),
});

function bounded(value, definition) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric)) return definition.default;
  return Math.min(definition.max, Math.max(definition.min, numeric));
}

function boundedPair(value, definition) {
  if (!Array.isArray(value) || value.length < 2) return definition.default;
  const x = Number(value[0]);
  const y = Number(value[1]);
  if (!Number.isFinite(x) || !Number.isFinite(y)) return definition.default;
  return [Math.min(definition.max, Math.max(definition.min, x)), Math.min(definition.max, Math.max(definition.min, y))];
}

function smooth(current, target, deltaTime, rate) {
  const amount = 1 - Math.pow(0.001, Math.max(0, Math.min(deltaTime, 0.25)) * rate);
  return current + (target - current) * amount;
}

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const uOriginLocation = gl.getUniformLocation(binding.program, 'uOrigin');
  const uProgressLocation = gl.getUniformLocation(binding.program, 'uProgress');
  const uWidthLocation = gl.getUniformLocation(binding.program, 'uWidth');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uOriginDefinition = manifest.uniforms.uOrigin;
  const uProgressDefinition = manifest.uniforms.uProgress;
  const uWidthDefinition = manifest.uniforms.uWidth;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uOrigin = boundedPair(initialParameters.uOrigin, uOriginDefinition);
  let target_uProgress = bounded(initialParameters.uProgress, uProgressDefinition);
  let target_uWidth = bounded(initialParameters.uWidth, uWidthDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uOrigin_x = target_uOrigin[0];
  let uOrigin_y = target_uOrigin[1];
  let uProgress = target_uProgress;
  let uWidth = target_uWidth;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uOrigin')) target_uOrigin = boundedPair(next.uOrigin, uOriginDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uProgress')) target_uProgress = bounded(next.uProgress, uProgressDefinition);
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
    uOrigin_x = smooth(uOrigin_x, target_uOrigin[0], deltaTime, 30);
    uOrigin_y = smooth(uOrigin_y, target_uOrigin[1], deltaTime, 30);
    uProgress = smooth(uProgress, target_uProgress, deltaTime, 16);
    uWidth = smooth(uWidth, target_uWidth, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uOriginLocation !== null) gl.uniform2f(uOriginLocation, uOrigin_x, uOrigin_y);
    if (uProgressLocation !== null) gl.uniform1f(uProgressLocation, uProgress);
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
  const attribute = 'data-click-ripple-fallback';
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
