import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'fluid-sim',
  title: 'Fluid Simulation',
  composition: Object.freeze({ slot: 'ambient', blend: 'alpha', zIndex: 14, priority: 14, exclusiveGroup: 'background-ambient' }),
  uniforms: Object.freeze({
    uIntensity: Object.freeze({ type: 'float', default: 0.24, min: 0, max: 0.48, step: 0.01, description: 'Fluid veil opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.26, min: 0, max: 1.25, step: 0.01, description: 'Fixed-clock advection speed.' }),
    uViscosity: Object.freeze({ type: 'float', default: 0.62, min: 0.15, max: 1.4, step: 0.01, description: 'Eddy smoothing and scale.' }),
    uTurbulence: Object.freeze({ type: 'float', default: 0.7, min: 0, max: 1.5, step: 0.01, description: 'Analytic curl displacement.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ renderScale: 1, detailScale: 1, alphaCap: 1 }),
    medium: Object.freeze({ renderScale: 0.75, detailScale: 0.76, alphaCap: 0.8 }),
    low: Object.freeze({ renderScale: 0.5, detailScale: 0.5, alphaCap: 0.58 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 3, medium: 1.9, low: 0.85 }),
    gpuMilliseconds: Object.freeze({ high: 1.25, medium: 0.76, low: 0.4 }),
    fill: 'fullscreen', allocation: 'steady-state-zero-js', estimatedDrawCalls: 1, textureBytes: 0,
  }),
  threading: Object.freeze({ instructionGeneration: 'worker-safe', render: 'main-or-offscreen' }),
});

function bounded(value, definition) {
  const numeric = Number(value);
  return Number.isFinite(numeric) ? Math.min(definition.max, Math.max(definition.min, numeric)) : definition.default;
}

function smooth(current, target, deltaTime) {
  const amount = 1 - Math.pow(0.001, Math.max(0, Math.min(deltaTime, 0.25)));
  return current + (target - current) * amount;
}

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const names = Object.keys(manifest.uniforms);
  const locations = Object.create(null);
  const target = Object.create(null);
  const current = Object.create(null);
  for (let index = 0; index < names.length; index += 1) {
    const name = names[index];
    locations[name] = gl.getUniformLocation(binding.program, name);
    target[name] = bounded(initialParameters[name], manifest.uniforms[name]);
    current[name] = target[name];
  }
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    for (let index = 0; index < names.length; index += 1) {
      const name = names[index];
      if (Object.prototype.hasOwnProperty.call(next, name)) target[name] = bounded(next[name], manifest.uniforms[name]);
    }
  }

  function setQuality(quality, admitted) {
    profile = manifest.quality[quality] || manifest.quality.medium;
    active = Boolean(admitted);
  }

  function resize() {}

  function simulate(deltaTime) {
    for (let index = 0; index < names.length; index += 1) {
      const name = names[index];
      current[name] = smooth(current[name], target[name], deltaTime);
    }
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    for (let index = 0; index < names.length; index += 1) {
      const name = names[index];
      const location = locations[name];
      if (location === null) continue;
      let value = current[name];
      if (name === 'uIntensity') value *= profile.alphaCap;
      if (name === 'uTurbulence') value *= profile.detailScale;
      gl.uniform1f(location, value);
    }
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
  const attribute = 'data-aurora-fluid-sim-fallback';
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
