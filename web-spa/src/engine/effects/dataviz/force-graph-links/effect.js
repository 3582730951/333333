import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'force-graph-links',
  title: 'Force Graph Links',
  composition: Object.freeze({slot: 'ambient',blend: 'additive',zIndex: 19,priority: 19,exclusiveGroup: 'dataviz-graph'}),
  uniforms: Object.freeze({
    uNodes: Object.freeze({ type: 'float', default: 10, min: 2, max: 10, step: 1, description: 'Active node count (scales link brightness).' }),
    uLinkWidth: Object.freeze({ type: 'float', default: 0.006, min: 0.002, max: 0.03, step: 0.001, description: 'Link glow width.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.32, min: 0, max: 0.8, step: 0.01, description: 'Link opacity.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({renderScale:1,alphaCap:1}),
    medium: Object.freeze({renderScale:0.7,alphaCap:0.8}),
    low: Object.freeze({renderScale:0.5,alphaCap:0.5}),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({high:1.45,medium:0.95,low:0.5}),
    gpuMilliseconds: Object.freeze({high:0.52,medium:0.33,low:0.18}),
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
  const uNodesLocation = gl.getUniformLocation(binding.program, 'uNodes');
  const uLinkWidthLocation = gl.getUniformLocation(binding.program, 'uLinkWidth');
  const uIntensityLocation = gl.getUniformLocation(binding.program, 'uIntensity');
  const uNodesDefinition = manifest.uniforms.uNodes;
  const uLinkWidthDefinition = manifest.uniforms.uLinkWidth;
  const uIntensityDefinition = manifest.uniforms.uIntensity;
  let target_uNodes = bounded(initialParameters.uNodes, uNodesDefinition);
  let target_uLinkWidth = bounded(initialParameters.uLinkWidth, uLinkWidthDefinition);
  let target_uIntensity = bounded(initialParameters.uIntensity, uIntensityDefinition);
  let uNodes = target_uNodes;
  let uLinkWidth = target_uLinkWidth;
  let uIntensity = target_uIntensity;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uNodes')) target_uNodes = bounded(next.uNodes, uNodesDefinition);
    if (Object.prototype.hasOwnProperty.call(next, 'uLinkWidth')) target_uLinkWidth = bounded(next.uLinkWidth, uLinkWidthDefinition);
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
    uNodes = smooth(uNodes, target_uNodes, deltaTime, 6);
    uLinkWidth = smooth(uLinkWidth, target_uLinkWidth, deltaTime, 6);
    uIntensity = smooth(uIntensity, target_uIntensity, deltaTime, 6);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (uNodesLocation !== null) gl.uniform1f(uNodesLocation, uNodes);
    if (uLinkWidthLocation !== null) gl.uniform1f(uLinkWidthLocation, uLinkWidth);
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
  const attribute = 'data-force-graph-links-fallback';
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
