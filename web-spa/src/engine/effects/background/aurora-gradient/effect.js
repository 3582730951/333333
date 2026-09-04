import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'aurora-gradient',
  title: 'Aurora Gradient',
  composition: Object.freeze({
    slot: 'ambient',
    blend: 'alpha',
    zIndex: 13,
    priority: 13,
    exclusiveGroup: 'background-ambient',
  }),
  uniforms: Object.freeze({
    uIntensity: Object.freeze({ type: 'float', default: 0.28, min: 0, max: 0.5, step: 0.01, description: 'Curtain opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.24, min: 0, max: 1.2, step: 0.01, description: 'Fixed-clock curtain drift.' }),
    uSpread: Object.freeze({ type: 'float', default: 0.68, min: 0.25, max: 1.25, step: 0.01, description: 'Vertical curtain spread.' }),
    uDrift: Object.freeze({ type: 'float', default: 0.35, min: 0, max: 1, step: 0.01, description: 'Horizontal wave displacement.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ renderScale: 1, bandCount: 3, alphaCap: 1 }),
    medium: Object.freeze({ renderScale: 0.75, bandCount: 2, alphaCap: 0.8 }),
    low: Object.freeze({ renderScale: 0.5, bandCount: 1, alphaCap: 0.58 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 2.5, medium: 1.6, low: 0.75 }),
    gpuMilliseconds: Object.freeze({ high: 0.9, medium: 0.55, low: 0.3 }),
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

function smooth(current, target, deltaTime) {
  const amount = 1 - Math.pow(0.001, Math.max(0, Math.min(deltaTime, 0.25)));
  return current + (target - current) * amount;
}

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({
    vertexSource: VERTEX_SOURCE,
    fragmentSource: FRAGMENT_SOURCE,
    label: manifest.id,
  });
  const intensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const speedLocation = gl.getUniformLocation(binding.program, 'uSpeed');
  const spreadLocation = gl.getUniformLocation(binding.program, 'uSpread');
  const driftLocation = gl.getUniformLocation(binding.program, 'uDrift');
  const intensityDefinition = manifest.uniforms.uIntensity;
  const speedDefinition = manifest.uniforms.uSpeed;
  const spreadDefinition = manifest.uniforms.uSpread;
  const driftDefinition = manifest.uniforms.uDrift;
  let targetIntensity = bounded(initialParameters.uIntensity, intensityDefinition);
  let targetSpeed = bounded(initialParameters.uSpeed, speedDefinition);
  let targetSpread = bounded(initialParameters.uSpread, spreadDefinition);
  let targetDrift = bounded(initialParameters.uDrift, driftDefinition);
  let intensity = targetIntensity;
  let speed = targetSpeed;
  let spread = targetSpread;
  let drift = targetDrift;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, intensityDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = bounded(next.uSpeed, speedDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpread')) targetSpread = bounded(next.uSpread, spreadDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uDrift')) targetDrift = bounded(next.uDrift, driftDefinition);
  }

  function setQuality(quality, admitted) {
    profile = manifest.quality[quality] || manifest.quality.medium;
    active = Boolean(admitted);
  }

  function resize() {
    // The host supplies the effective, quality-capped resolution per frame.
  }

  function simulate(deltaTime) {
    intensity = smooth(intensity, targetIntensity, deltaTime);
    speed = smooth(speed, targetSpeed, deltaTime);
    spread = smooth(spread, targetSpread, deltaTime);
    drift = smooth(drift, targetDrift, deltaTime);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (intensityLocation !== null) gl.uniform1f(intensityLocation, intensity * profile.alphaCap);
    if (speedLocation !== null) gl.uniform1f(speedLocation, speed);
    if (spreadLocation !== null) gl.uniform1f(spreadLocation, spread);
    if (driftLocation !== null) gl.uniform1f(driftLocation, drift);
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
  const attribute = 'data-aurora-gradient-fallback';
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
