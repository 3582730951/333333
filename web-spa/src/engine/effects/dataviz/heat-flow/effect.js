import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'heat-flow',
  title: 'Heat Flow',
  composition: Object.freeze({slot: 'ambient',blend: 'alpha',zIndex: 16,priority: 16,exclusiveGroup: 'dataviz-field'}),
  uniforms: Object.freeze({
    uScale: Object.freeze({ type: 'float', default: 4, min: 1, max: 12, step: 0.1, description: 'Noise field scale.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.3, min: 0, max: 1.2, step: 0.01, description: 'Advection speed.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.3, min: 0, max: 0.8, step: 0.01, description: 'Heat opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.7,alphaCap:0.8}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.55}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:1.6,medium:1.05,low:0.55}),
    gpuMilliseconds: Object.freeze({high:0.58,medium:0.36,low:0.2}),
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
  const uScaleLocation = gl.getUniformLocation(binding.program, 'uScale');
  const uSpeedLocation = gl.getUniformLocation(binding.program, 'uSpeed');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uScaleDefinition = manifest.uniforms.uScale;
  const uSpeedDefinition = manifest.uniforms.uSpeed;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uScale = bounded(initialParameters.uScale, uScaleDefinition);
  let target_uSpeed = bounded(initialParameters.uSpeed, uSpeedDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uScale = target_uScale;
  let uSpeed = target_uSpeed;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uScale')) target_uScale = bounded(next.uScale, uScaleDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) target_uSpeed = bounded(next.uSpeed, uSpeedDefinition);
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
    uScale = smooth(uScale, target_uScale, deltaTime, 6);
    uSpeed = smooth(uSpeed, target_uSpeed, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uScaleLocation !== null) gl.uniform1f(uScaleLocation, uScale);
    if (uSpeedLocation !== null) gl.uniform1f(uSpeedLocation, uSpeed);
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
  const attribute = 'data-heat-flow-fallback';
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
