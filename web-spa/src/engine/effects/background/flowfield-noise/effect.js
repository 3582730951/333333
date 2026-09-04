import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'flowfield-noise',
  title: 'Flowfield Noise',
  composition: Object.freeze({
    slot: 'ambient',
    blend: 'alpha',
    zIndex: 12,
    priority: 12,
    exclusiveGroup: 'background-ambient',
  }),
  uniforms: Object.freeze({
    uIntensity: Object.freeze({ type: 'float', default: 0.22, min: 0, max: 0.45, step: 0.01, description: 'Overall field opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.32, min: 0, max: 1.5, step: 0.01, description: 'Fixed-clock field travel speed.' }),
    uScale: Object.freeze({ type: 'float', default: 1.05, min: 0.45, max: 2.4, step: 0.01, description: 'Noise-cell scale and filament density.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ renderScale: 1, detailScale: 1, alphaCap: 1 }),
    medium: Object.freeze({ renderScale: 0.75, detailScale: 0.78, alphaCap: 0.8 }),
    low: Object.freeze({ renderScale: 0.5, detailScale: 0.56, alphaCap: 0.58 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 3, medium: 2, low: 0.9 }),
    gpuMilliseconds: Object.freeze({ high: 1.35, medium: 0.82, low: 0.42 }),
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
  const scaleLocation = gl.getUniformLocation(binding.program, 'uScale');
  const intensityDefinition = manifest.uniforms.uIntensity;
  const speedDefinition = manifest.uniforms.uSpeed;
  const scaleDefinition = manifest.uniforms.uScale;
  let targetIntensity = bounded(initialParameters.uIntensity, intensityDefinition);
  let targetSpeed = bounded(initialParameters.uSpeed, speedDefinition);
  let targetScale = bounded(initialParameters.uScale, scaleDefinition);
  let intensity = targetIntensity;
  let speed = targetSpeed;
  let scale = targetScale;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, intensityDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = bounded(next.uSpeed, speedDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uScale')) targetScale = bounded(next.uScale, scaleDefinition);
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
    scale = smooth(scale, targetScale, deltaTime);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (intensityLocation !== null) gl.uniform1f(intensityLocation, intensity * profile.alphaCap);
    if (speedLocation !== null) gl.uniform1f(speedLocation, speed);
    if (scaleLocation !== null) gl.uniform1f(scaleLocation, scale * profile.detailScale);
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
  const attribute = 'data-aurora-flowfield-noise-fallback';
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
