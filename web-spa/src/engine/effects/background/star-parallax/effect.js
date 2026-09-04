import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'star-parallax',
  title: 'Star Parallax',
  composition: Object.freeze({ slot: 'ambient', blend: 'alpha', zIndex: 15, priority: 15, exclusiveGroup: 'background-ambient' }),
  uniforms: Object.freeze({
    uIntensity: Object.freeze({ type: 'float', default: 0.2, min: 0, max: 0.46, step: 0.01, description: 'Starfield opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.16, min: 0, max: 1, step: 0.01, description: 'Fixed-clock layer drift.' }),
    uDensity: Object.freeze({ type: 'float', default: 0.72, min: 0.2, max: 1.5, step: 0.01, description: 'Cell density for procedural stars.' }),
    uParallax: Object.freeze({ type: 'float', default: 0, min: -1, max: 1, step: 0.01, description: 'Caller-smoothed horizontal parallax offset.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ renderScale: 1, detailScale: 1, alphaCap: 1 }),
    medium: Object.freeze({ renderScale: 0.75, detailScale: 0.73, alphaCap: 0.8 }),
    low: Object.freeze({ renderScale: 0.5, detailScale: 0.48, alphaCap: 0.58 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 2.2, medium: 1.3, low: 0.55 }),
    gpuMilliseconds: Object.freeze({ high: 0.78, medium: 0.45, low: 0.24 }),
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
      if (name === 'uDensity') value *= profile.detailScale;
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
  const attribute = 'data-aurora-star-parallax-fallback';
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
