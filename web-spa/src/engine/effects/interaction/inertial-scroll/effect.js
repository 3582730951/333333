import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'inertial-scroll',
  title: 'Inertial Scroll',
  composition: Object.freeze({slot: 'ambient',blend: 'alpha',zIndex: 22,priority: 22,exclusiveGroup: 'scroll-feedback'}),
  uniforms: Object.freeze({
    uVelocity: Object.freeze({ type: 'float', default: 0, min: -1, max: 1, step: 0.01, description: 'Normalised scroll velocity.' }),
    uOverscroll: Object.freeze({ type: 'float', default: 0, min: -1, max: 1, step: 0.01, description: 'Overscroll displacement past the edge.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.32, min: 0, max: 0.8, step: 0.01, description: 'Cue opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.7,alphaCap:0.8}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.55}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.22,medium:0.15,low:0.08}),
    gpuMilliseconds: Object.freeze({high:0.08,medium:0.05,low:0.03}),
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

function smooth(current, target, deltaTime, rate) {
  const amount = 1 - Math.pow(0.001, Math.max(0, Math.min(deltaTime, 0.25)) * rate);
  return current + (target - current) * amount;
}

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const uVelocityLocation = gl.getUniformLocation(binding.program, 'uVelocity');
  const uOverscrollLocation = gl.getUniformLocation(binding.program, 'uOverscroll');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uVelocityDefinition = manifest.uniforms.uVelocity;
  const uOverscrollDefinition = manifest.uniforms.uOverscroll;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uVelocity = bounded(initialParameters.uVelocity, uVelocityDefinition);
  let target_uOverscroll = bounded(initialParameters.uOverscroll, uOverscrollDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uVelocity = target_uVelocity;
  let uOverscroll = target_uOverscroll;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uVelocity')) target_uVelocity = bounded(next.uVelocity, uVelocityDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uOverscroll')) target_uOverscroll = bounded(next.uOverscroll, uOverscrollDefinition);
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
    uVelocity = smooth(uVelocity, target_uVelocity, deltaTime, 22);
    uOverscroll = smooth(uOverscroll, target_uOverscroll, deltaTime, 18);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uVelocityLocation !== null) gl.uniform1f(uVelocityLocation, uVelocity);
    if (uOverscrollLocation !== null) gl.uniform1f(uOverscrollLocation, uOverscroll);
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
  const attribute = 'data-inertial-scroll-fallback';
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
