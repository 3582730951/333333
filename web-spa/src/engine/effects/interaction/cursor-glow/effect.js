import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'cursor-glow',
  title: 'Cursor Glow',
  composition: Object.freeze({slot: 'interaction',blend: 'additive',zIndex: 41,priority: 41,exclusiveGroup: 'pointer-affordance'}),
  uniforms: Object.freeze({
    uPointer: Object.freeze({ type: 'vec2', default: [0.5,0.5], min: 0, max: 1, step: 0.001, description: 'Pointer position in effect UV space.' }),
    uRadius: Object.freeze({ type: 'float', default: 0.09, min: 0.02, max: 0.4, step: 0.005, description: 'Core halo radius.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.42, min: 0, max: 1, step: 0.01, description: 'Halo opacity.' }),
    uTrail: Object.freeze({ type: 'float', default: 0.55, min: 0, max: 1, step: 0.01, description: 'Weight of the wide trailing lobe.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.75,alphaCap:0.85}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.6}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:0.3,medium:0.2,low:0.12}),
    gpuMilliseconds: Object.freeze({high:0.11,medium:0.07,low:0.04}),
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
  const uPointerLocation = gl.getUniformLocation(binding.program, 'uPointer');
  const uRadiusLocation = gl.getUniformLocation(binding.program, 'uRadius');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uTrailLocation = gl.getUniformLocation(binding.program, 'uTrail');
  const uPointerDefinition = manifest.uniforms.uPointer;
  const uRadiusDefinition = manifest.uniforms.uRadius;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  const uTrailDefinition = manifest.uniforms.uTrail;
  let target_uPointer = boundedPair(initialParameters.uPointer, uPointerDefinition);
  let target_uRadius = bounded(initialParameters.uRadius, uRadiusDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let target_uTrail = bounded(initialParameters.uTrail, uTrailDefinition);
  let uPointer_x = target_uPointer[0];
  let uPointer_y = target_uPointer[1];
  let uRadius = target_uRadius;
  let uIntensity = target_uIntensity;
  let uTrail = target_uTrail;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uPointer')) target_uPointer = boundedPair(next.uPointer, uPointerDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uRadius')) target_uRadius = bounded(next.uRadius, uRadiusDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) target_uIntensity = bounded(next.uIntensity, uIntensityDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uTrail')) target_uTrail = bounded(next.uTrail, uTrailDefinition);
  }

  function setQuality(quality, admitted) {
    profile = manifest.quality[quality] || manifest.quality.medium;
    active = Boolean(admitted);
  }

  function resize() {
    // Host owns the quality-capped resolution; nothing per-effect to recompute.
  }

  function simulate(deltaTime) {
    uPointer_x = smooth(uPointer_x, target_uPointer[0], deltaTime, 20);
    uPointer_y = smooth(uPointer_y, target_uPointer[1], deltaTime, 20);
    uRadius = smooth(uRadius, target_uRadius, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
    uTrail = smooth(uTrail, target_uTrail, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uPointerLocation !== null) gl.uniform2f(uPointerLocation, uPointer_x, uPointer_y);
    if (uRadiusLocation !== null) gl.uniform1f(uRadiusLocation, uRadius);
    if (uIntensityLocation !== null) gl.uniform1f(uIntensityLocation, uIntensity * profile.alphaCap);
    if (uTrailLocation !== null) gl.uniform1f(uTrailLocation, uTrail);
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
  const attribute = 'data-cursor-glow-fallback';
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
