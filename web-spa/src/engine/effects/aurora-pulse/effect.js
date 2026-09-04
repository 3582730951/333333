import { clampUnit, ENGINE_EFFECT_SCHEMA_VERSION } from '../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'aurora-pulse',
  title: 'Aurora Pulse',
  composition: Object.freeze({
    slot: 'ambient',
    blend: 'alpha',
    zIndex: 20,
    priority: 20,
    exclusiveGroup: 'ambient-pulse',
  }),
  // These are effect-owned uniforms. Engine-owned global uniforms (uTime,
  // uResolution, palette uniforms, and quality) are intentionally absent.
  uniforms: Object.freeze({
    uIntensity: Object.freeze({ type: 'float', default: 0.18, min: 0, max: 0.5 }),
    uSpeed: Object.freeze({ type: 'float', default: 0.7, min: 0, max: 3 }),
    uAmplitude: Object.freeze({ type: 'float', default: 0.13, min: 0.02, max: 0.3 }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ ringDensity: 1, alphaCap: 1 }),
    medium: Object.freeze({ ringDensity: 0.72, alphaCap: 0.82 }),
    low: Object.freeze({ ringDensity: 0.45, alphaCap: 0.56 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 2, medium: 1.4, low: 0.8 }),
    gpuMilliseconds: Object.freeze({ high: 0.35, medium: 0.22, low: 0.12 }),
    fill: 'fullscreen',
    allocation: 'steady-state-zero-js',
  }),
  threading: Object.freeze({
    instructionGeneration: 'worker-safe',
    render: 'main-or-offscreen',
  }),
});

function bounded(value, definition) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric)) return definition.default;
  return Math.min(definition.max, Math.max(definition.min, numeric));
}

/**
 * Creates GPU state only after the lazy registry has loaded this directory.
 * The instance never owns the canvas or clears the framebuffer: the compositor
 * controls both so sibling effects cannot erase or reorder one another.
 */
export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({
    vertexSource: VERTEX_SOURCE,
    fragmentSource: FRAGMENT_SOURCE,
    label: manifest.id,
  });
  const intensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const speedLocation = gl.getUniformLocation(binding.program, 'uSpeed');
  const amplitudeLocation = gl.getUniformLocation(binding.program, 'uAmplitude');
  const intensityDefinition = manifest.uniforms.uIntensity;
  const speedDefinition = manifest.uniforms.uSpeed;
  const amplitudeDefinition = manifest.uniforms.uAmplitude;
  let targetIntensity = bounded(initialParameters.uIntensity, intensityDefinition);
  let currentIntensity = targetIntensity;
  let speed = bounded(initialParameters.uSpeed, speedDefinition);
  let amplitude = bounded(initialParameters.uAmplitude, amplitudeDefinition);
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) {
      targetIntensity = bounded(next.uIntensity, intensityDefinition);
    }
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) speed = bounded(next.uSpeed, speedDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uAmplitude')) amplitude = bounded(next.uAmplitude, amplitudeDefinition);
  }

  function setQuality(quality, admitted) {
    profile = manifest.quality[quality] || manifest.quality.medium;
    active = Boolean(admitted);
  }

  function simulate(deltaTime) {
    const easing = 1 - Math.pow(0.0001, Math.max(0, deltaTime));
    currentIntensity += (targetIntensity - currentIntensity) * easing;
  }

  function resize() {
    // This fullscreen shader uses engine-injected uResolution at render time.
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (intensityLocation !== null) gl.uniform1f(intensityLocation, clampUnit(currentIntensity) * profile.alphaCap);
    if (speedLocation !== null) gl.uniform1f(speedLocation, speed);
    if (amplitudeLocation !== null) gl.uniform1f(amplitudeLocation, amplitude * profile.ringDensity);
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

/**
 * Fallback must be safe even when it is the only effect code already loaded.
 * The app's ordinary DOM is the functional path; this hook merely exposes a
 * deterministic data attribute for a page-owned CSS/DOM fallback if it exists.
 */
export function applyDomFallback(root) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  root.setAttribute('data-aurora-pulse-fallback', 'true');
  return () => root.removeAttribute('data-aurora-pulse-fallback');
}
