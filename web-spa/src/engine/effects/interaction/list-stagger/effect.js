import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'list-stagger',
  title: 'List Stagger',
  composition: Object.freeze({slot: 'ambient',blend: 'alpha',zIndex: 24,priority: 24,exclusiveGroup: 'list-entrance'}),
  uniforms: Object.freeze({
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, step: 0.01, description: 'Entrance envelope owned by the host.' }),
    uRows: Object.freeze({ type: 'float', default: 8, min: 1, max: 40, step: 1, description: 'Row count the list is showing.' }),
    uStagger: Object.freeze({ type: 'float', default: 0.45, min: 0, max: 0.9, step: 0.01, description: 'Total stagger spread across rows.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.3, min: 0, max: 0.8, step: 0.01, description: 'Entrance opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.75,alphaCap:0.82}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.55}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.25,medium:0.17,low:0.1}),
    gpuMilliseconds: Object.freeze({high:0.09,medium:0.06,low:0.035}),
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
  const uRowsLocation = gl.getUniformLocation(binding.program, 'uRows');
  const uStaggerLocation = gl.getUniformLocation(binding.program, 'uStagger');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uProgressDefinition = manifest.uniforms.uProgress;
  const uRowsDefinition = manifest.uniforms.uRows;
  const uStaggerDefinition = manifest.uniforms.uStagger;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uProgress = bounded(initialParameters.uProgress, uProgressDefinition);
  let target_uRows = bounded(initialParameters.uRows, uRowsDefinition);
  let target_uStagger = bounded(initialParameters.uStagger, uStaggerDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uProgress = target_uProgress;
  let uRows = target_uRows;
  let uStagger = target_uStagger;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uProgress')) target_uProgress = bounded(next.uProgress, uProgressDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uRows')) target_uRows = bounded(next.uRows, uRowsDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uStagger')) target_uStagger = bounded(next.uStagger, uStaggerDefinition);
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
    uProgress = smooth(uProgress, target_uProgress, deltaTime, 9);
    uRows = smooth(uRows, target_uRows, deltaTime, 6);
    uStagger = smooth(uStagger, target_uStagger, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uProgressLocation !== null) gl.uniform1f(uProgressLocation, uProgress);
    if (uRowsLocation !== null) gl.uniform1f(uRowsLocation, uRows);
    if (uStaggerLocation !== null) gl.uniform1f(uStaggerLocation, uStagger);
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
  const attribute = 'data-list-stagger-fallback';
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
