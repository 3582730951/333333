import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'press-elastic',
  title: 'Press Elastic',
  composition: Object.freeze({slot: 'interaction',blend: 'additive',zIndex: 42,priority: 42,exclusiveGroup: 'pointer-press'}),
  uniforms: Object.freeze({
    uOrigin: Object.freeze({ type: 'vec2', default: [0.5,0.5], min: 0, max: 1, step: 0.001, description: 'Press origin in effect UV space.' }),
    uPress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, step: 0.01, description: 'Press envelope, 0 released to 1 fully pressed.' }),
    uSpring: Object.freeze({ type: 'float', default: 0.6, min: 0, max: 1.5, step: 0.01, description: 'Overshoot amount of the elastic settle.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.5, min: 0, max: 1, step: 0.01, description: 'Ring opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.75,alphaCap:0.85}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.6}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.26,medium:0.18,low:0.1}),
    gpuMilliseconds: Object.freeze({high:0.1,medium:0.06,low:0.035}),
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
  const uPressLocation = gl.getUniformLocation(binding.program, 'uPress');
  const uSpringLocation = gl.getUniformLocation(binding.program, 'uSpring');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uOriginDefinition = manifest.uniforms.uOrigin;
  const uPressDefinition = manifest.uniforms.uPress;
  const uSpringDefinition = manifest.uniforms.uSpring;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uOrigin = boundedPair(initialParameters.uOrigin, uOriginDefinition);
  let target_uPress = bounded(initialParameters.uPress, uPressDefinition);
  let target_uSpring = bounded(initialParameters.uSpring, uSpringDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uOrigin_x = target_uOrigin[0];
  let uOrigin_y = target_uOrigin[1];
  let uPress = target_uPress;
  let uSpring = target_uSpring;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uOrigin')) target_uOrigin = boundedPair(next.uOrigin, uOriginDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uPress')) target_uPress = bounded(next.uPress, uPressDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpring')) target_uSpring = bounded(next.uSpring, uSpringDefinition);
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
    uOrigin_x = smooth(uOrigin_x, target_uOrigin[0], deltaTime, 24);
    uOrigin_y = smooth(uOrigin_y, target_uOrigin[1], deltaTime, 24);
    uPress = smooth(uPress, target_uPress, deltaTime, 14);
    uSpring = smooth(uSpring, target_uSpring, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uOriginLocation !== null) gl.uniform2f(uOriginLocation, uOrigin_x, uOrigin_y);
    if (uPressLocation !== null) gl.uniform1f(uPressLocation, uPress);
    if (uSpringLocation !== null) gl.uniform1f(uSpringLocation, uSpring);
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
  const attribute = 'data-press-elastic-fallback';
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
