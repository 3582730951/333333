import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'skeleton-shimmer',
  title: 'Skeleton Shimmer',
  composition: Object.freeze({slot: 'ambient',blend: 'alpha',zIndex: 20,priority: 20,exclusiveGroup: 'loading-state'}),
  uniforms: Object.freeze({
    uSpeed: Object.freeze({ type: 'float', default: 0.55, min: 0.1, max: 2, step: 0.01, description: 'Sweep cycles per second.' }),
    uWidth: Object.freeze({ type: 'float', default: 0.22, min: 0.05, max: 0.6, step: 0.01, description: 'Sweep band width.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.26, min: 0, max: 0.7, step: 0.01, description: 'Shimmer opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.7,alphaCap:0.8}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.5}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.2,medium:0.13,low:0.08}),
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
  const uSpeedLocation = gl.getUniformLocation(binding.program, 'uSpeed');
  const uWidthLocation = gl.getUniformLocation(binding.program, 'uWidth');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uSpeedDefinition = manifest.uniforms.uSpeed;
  const uWidthDefinition = manifest.uniforms.uWidth;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uSpeed = bounded(initialParameters.uSpeed, uSpeedDefinition);
  let target_uWidth = bounded(initialParameters.uWidth, uWidthDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uSpeed = target_uSpeed;
  let uWidth = target_uWidth;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) target_uSpeed = bounded(next.uSpeed, uSpeedDefinition);
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
    uSpeed = smooth(uSpeed, target_uSpeed, deltaTime, 6);
    uWidth = smooth(uWidth, target_uWidth, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uSpeedLocation !== null) gl.uniform1f(uSpeedLocation, uSpeed);
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
  const attribute = 'data-skeleton-shimmer-fallback';
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
