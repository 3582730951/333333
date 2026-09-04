import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'magnetic-button',
  title: 'Magnetic Button',
  composition: Object.freeze({
    slot: 'interaction',
    blend: 'additive',
    zIndex: 40,
    priority: 40,
    exclusiveGroup: 'pointer-affordance',
  }),
  uniforms: Object.freeze({
    uPointer: Object.freeze({ type: 'vec2', default: [0.5, 0.5], min: 0, max: 1, step: 0.001, description: 'Pointer position in effect UV space.' }),
    uPull: Object.freeze({ type: 'float', default: 0.55, min: 0, max: 1, step: 0.01, description: 'Falloff sharpness of the attraction field.' }),
    uRadius: Object.freeze({ type: 'float', default: 0.28, min: 0.05, max: 0.8, step: 0.01, description: 'Field radius in normalised units.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.34, min: 0, max: 0.8, step: 0.01, description: 'Peak field opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ renderScale: 1, alphaCap: 1, followRate: 1 }),
    medium: Object.freeze({ renderScale: 0.75, alphaCap: 0.85, followRate: 0.8 }),
    low: Object.freeze({ renderScale: 0.5, alphaCap: 0.6, followRate: 0.55 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 0.38, medium: 0.26, low: 0.15 }),
    gpuMilliseconds: Object.freeze({ high: 0.14, medium: 0.09, low: 0.05 }),
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
  const pointerLocation = gl.getUniformLocation(binding.program, 'uPointer');
  const pullLocation = gl.getUniformLocation(binding.program, 'uPull');
  const radiusLocation = gl.getUniformLocation(binding.program, 'uRadius');
  const intensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const pointerDefinition = manifest.uniforms.uPointer;
  const pullDefinition = manifest.uniforms.uPull;
  const radiusDefinition = manifest.uniforms.uRadius;
  const intensityDefinition = manifest.uniforms.uIntensity;

  let targetPointer = boundedPair(initialParameters.uPointer, pointerDefinition);
  let targetPull = bounded(initialParameters.uPull, pullDefinition);
  let targetRadius = bounded(initialParameters.uRadius, radiusDefinition);
  let targetIntensity = bounded(initialParameters.uIntensity, intensityDefinition);
  let pointerX = targetPointer[0];
  let pointerY = targetPointer[1];
  let pull = targetPull;
  let radius = targetRadius;
  let intensity = targetIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uPointer')) targetPointer = boundedPair(next.uPointer, pointerDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uPull')) targetPull = bounded(next.uPull, pullDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uRadius')) targetRadius = bounded(next.uRadius, radiusDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, intensityDefinition);
  }

  function setQuality(quality, admitted) {
    profile = manifest.quality[quality] || manifest.quality.medium;
    active = Boolean(admitted);
  }

  function resize() {
    // Host owns the quality-capped resolution; nothing per-effect to recompute.
  }

  function simulate(deltaTime) {
    // R4: the pointer must feel attached, so it tracks fastest and is the one
    // value low quality slows down rather than drops.
    pointerX = smooth(pointerX, targetPointer[0], deltaTime, profile.followRate * 18);
    pointerY = smooth(pointerY, targetPointer[1], deltaTime, profile.followRate * 18);
    pull = smooth(pull, targetPull, deltaTime, 6);
    radius = smooth(radius, targetRadius, deltaTime, 6);
    intensity = smooth(intensity, targetIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (pointerLocation !== null) gl.uniform2f(pointerLocation, pointerX, pointerY);
    if (pullLocation !== null) gl.uniform1f(pullLocation, pull);
    if (radiusLocation !== null) gl.uniform1f(radiusLocation, radius);
    if (intensityLocation !== null) gl.uniform1f(intensityLocation, intensity * profile.alphaCap);
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
  const attribute = 'data-magnetic-button-fallback';
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
