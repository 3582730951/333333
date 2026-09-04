import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'hover-parallax-tilt',
  title: 'Hover Parallax Tilt',
  composition: Object.freeze({slot: 'interaction',blend: 'additive',zIndex: 39,priority: 39,exclusiveGroup: 'pointer-affordance'}),
  uniforms: Object.freeze({
    uTilt: Object.freeze({ type: 'vec2', default: [0,0], min: -1, max: 1, step: 0.001, description: 'Normalised tilt vector from pointer offset.' }),
    uDepth: Object.freeze({ type: 'float', default: 0.6, min: 0, max: 1.5, step: 0.01, description: 'Parallax depth of the specular band.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.3, min: 0, max: 0.8, step: 0.01, description: 'Sheen opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.75,alphaCap:0.82}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.55}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.28,medium:0.19,low:0.11}),
    gpuMilliseconds: Object.freeze({high:0.1,medium:0.07,low:0.04}),
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
  const uTiltLocation = gl.getUniformLocation(binding.program, 'uTilt');
  const uDepthLocation = gl.getUniformLocation(binding.program, 'uDepth');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uTiltDefinition = manifest.uniforms.uTilt;
  const uDepthDefinition = manifest.uniforms.uDepth;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uTilt = boundedPair(initialParameters.uTilt, uTiltDefinition);
  let target_uDepth = bounded(initialParameters.uDepth, uDepthDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uTilt_x = target_uTilt[0];
  let uTilt_y = target_uTilt[1];
  let uDepth = target_uDepth;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uTilt')) target_uTilt = boundedPair(next.uTilt, uTiltDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uDepth')) target_uDepth = bounded(next.uDepth, uDepthDefinition);
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
    uTilt_x = smooth(uTilt_x, target_uTilt[0], deltaTime, 16);
    uTilt_y = smooth(uTilt_y, target_uTilt[1], deltaTime, 16);
    uDepth = smooth(uDepth, target_uDepth, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uTiltLocation !== null) gl.uniform2f(uTiltLocation, uTilt_x, uTilt_y);
    if (uDepthLocation !== null) gl.uniform1f(uDepthLocation, uDepth);
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
  const attribute = 'data-hover-parallax-tilt-fallback';
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
