import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'liquid-metal',
  title: 'Liquid Metal',
  composition: Object.freeze({ slot: 'ambient', blend: 'alpha', zIndex: 17, priority: 17, exclusiveGroup: 'background-ambient' }),
  uniforms: Object.freeze({
    uIntensity: Object.freeze({ type: 'float', default: 0.2, min: 0, max: 0.45, step: 0.01, description: 'Reflective surface opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.3, min: 0, max: 1.4, step: 0.01, description: 'Fixed-clock liquid motion.' }),
    uRoughness: Object.freeze({ type: 'float', default: 0.38, min: 0.08, max: 0.9, step: 0.01, description: 'Reflection blur.' }),
    uDistortion: Object.freeze({ type: 'float', default: 0.42, min: 0, max: 1.2, step: 0.01, description: 'Surface normal distortion.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ renderScale: 1, detailScale: 1, alphaCap: 1 }),
    medium: Object.freeze({ renderScale: 0.75, detailScale: 0.74, alphaCap: 0.8 }),
    low: Object.freeze({ renderScale: 0.5, detailScale: 0.5, alphaCap: 0.58 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 2.6, medium: 1.65, low: 0.72 }),
    gpuMilliseconds: Object.freeze({ high: 1.05, medium: 0.64, low: 0.34 }),
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
      if (name === 'uDistortion') value *= profile.detailScale;
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
  const attribute = 'data-aurora-liquid-metal-fallback';
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
