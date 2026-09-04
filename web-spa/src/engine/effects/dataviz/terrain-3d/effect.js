import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'terrain-3d',
  title: 'Terrain 3D',
  composition: Object.freeze({slot: 'base',blend: 'alpha',zIndex: 12,priority: 12,exclusiveGroup: 'dataviz-field'}),
  uniforms: Object.freeze({
    uHeight: Object.freeze({ type: 'float', default: 0.35, min: 0, max: 1, step: 0.01, description: 'Ridge height scale.' }),
    uGrid: Object.freeze({ type: 'float', default: 6, min: 2, max: 20, step: 0.5, description: 'Grid line density along depth.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.28, min: 0, max: 0.8, step: 0.01, description: 'Terrain opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.7,alphaCap:0.78}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.5}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:1.75,medium:1.15,low:0.6}),
    gpuMilliseconds: Object.freeze({high:0.63,medium:0.4,low:0.21}),
    fill: 'fullscreen',
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
  const uHeightLocation = gl.getUniformLocation(binding.program, 'uHeight');
  const uGridLocation = gl.getUniformLocation(binding.program, 'uGrid');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uHeightDefinition = manifest.uniforms.uHeight;
  const uGridDefinition = manifest.uniforms.uGrid;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uHeight = bounded(initialParameters.uHeight, uHeightDefinition);
  let target_uGrid = bounded(initialParameters.uGrid, uGridDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uHeight = target_uHeight;
  let uGrid = target_uGrid;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uHeight')) target_uHeight = bounded(next.uHeight, uHeightDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uGrid')) target_uGrid = bounded(next.uGrid, uGridDefinition);
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
    uHeight = smooth(uHeight, target_uHeight, deltaTime, 6);
    uGrid = smooth(uGrid, target_uGrid, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uHeightLocation !== null) gl.uniform1f(uHeightLocation, uHeight);
    if (uGridLocation !== null) gl.uniform1f(uGridLocation, uGrid);
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
  const attribute = 'data-terrain-3d-fallback';
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
