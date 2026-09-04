import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'glass-refraction',
  title: 'Glass Refraction',
  composition: Object.freeze({
    slot: 'foreground',
    blend: 'alpha',
    zIndex: 42,
    priority: 42,
    exclusiveGroup: 'material-surface',
  }),
  uniforms: Object.freeze({
    uElementRect: Object.freeze({ type: 'vec4', default: [0.25, 0.25, 0.5, 0.5], description: 'Normalized [left, bottom, width, height] target rect.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.66, 0.85, 1.0], description: 'Glass tint mixed with the Aurora palette.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.22, min: 0, max: 0.5 }),
    uSpeed: Object.freeze({ type: 'float', default: 0.34, min: 0, max: 2 }),
    uRefraction: Object.freeze({ type: 'float', default: 0.018, min: 0, max: 0.06 }),
    uBlur: Object.freeze({ type: 'float', default: 0.58, min: 0, max: 1 }),
    uCornerRadius: Object.freeze({ type: 'float', default: 0.11, min: 0, max: 0.3 }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ waveDensity: 1, opacityCap: 1 }),
    medium: Object.freeze({ waveDensity: 0.72, opacityCap: 0.82 }),
    low: Object.freeze({ waveDensity: 0.42, opacityCap: 0.58 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 0.7, medium: 0.45, low: 0.22 }),
    gpuMilliseconds: Object.freeze({ high: 0.18, medium: 0.12, low: 0.07 }),
    fill: 'partial',
    allocation: 'steady-state-zero-js',
    estimatedDrawCalls: 1,
  }),
  threading: Object.freeze({ instructionGeneration: 'worker-safe', render: 'main-or-offscreen' }),
});

function number(value, definition) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric)) return definition.default;
  return Math.min(definition.max, Math.max(definition.min, numeric));
}

function component(value, index, fallback, minimum, maximum) {
  const numeric = Number(value && value[index]);
  return Number.isFinite(numeric) ? Math.min(maximum, Math.max(minimum, numeric)) : fallback[index];
}

function rect(value, fallback) {
  return [
    component(value, 0, fallback, -0.5, 1.5),
    component(value, 1, fallback, -0.5, 1.5),
    component(value, 2, fallback, 0.001, 2),
    component(value, 3, fallback, 0.001, 2),
  ];
}

function tint(value, fallback) {
  return [component(value, 0, fallback, 0, 1), component(value, 1, fallback, 0, 1), component(value, 2, fallback, 0, 1)];
}

function ease(current, target, deltaTime) {
  return current + (target - current) * (1 - Math.pow(0.0001, Math.max(0, deltaTime)));
}

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({
    rect: gl.getUniformLocation(binding.program, 'uElementRect'), tint: gl.getUniformLocation(binding.program, 'uTint'),
    intensity: gl.getUniformLocation(binding.program, 'uIntensity'), speed: gl.getUniformLocation(binding.program, 'uSpeed'),
    refraction: gl.getUniformLocation(binding.program, 'uRefraction'), blur: gl.getUniformLocation(binding.program, 'uBlur'),
    cornerRadius: gl.getUniformLocation(binding.program, 'uCornerRadius'), waveDensity: gl.getUniformLocation(binding.program, 'uWaveDensity'),
    opacityCap: gl.getUniformLocation(binding.program, 'uOpacityCap'),
  });
  const definitions = manifest.uniforms;
  let targetRect = rect(initialParameters.uElementRect, definitions.uElementRect.default);
  let currentRect = targetRect.slice();
  let targetTint = tint(initialParameters.uTint, definitions.uTint.default);
  let currentTint = targetTint.slice();
  let targetIntensity = number(initialParameters.uIntensity, definitions.uIntensity);
  let currentIntensity = targetIntensity;
  let targetSpeed = number(initialParameters.uSpeed, definitions.uSpeed);
  let currentSpeed = targetSpeed;
  let targetRefraction = number(initialParameters.uRefraction, definitions.uRefraction);
  let currentRefraction = targetRefraction;
  let targetBlur = number(initialParameters.uBlur, definitions.uBlur);
  let currentBlur = targetBlur;
  let targetCornerRadius = number(initialParameters.uCornerRadius, definitions.uCornerRadius);
  let currentCornerRadius = targetCornerRadius;
  let profile = manifest.quality.medium;
  let active = true;
  let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uElementRect')) targetRect = rect(next.uElementRect, targetRect);
    if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = tint(next.uTint, targetTint);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = number(next.uIntensity, definitions.uIntensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = number(next.uSpeed, definitions.uSpeed);
    if (Object.prototype.hasOwnProperty.call(next, 'uRefraction')) targetRefraction = number(next.uRefraction, definitions.uRefraction);
    if (Object.prototype.hasOwnProperty.call(next, 'uBlur')) targetBlur = number(next.uBlur, definitions.uBlur);
    if (Object.prototype.hasOwnProperty.call(next, 'uCornerRadius')) targetCornerRadius = number(next.uCornerRadius, definitions.uCornerRadius);
  }

  function setQuality(quality, admitted) {
    profile = manifest.quality[quality] || manifest.quality.medium;
    active = Boolean(admitted);
  }

  function resize() {}

  function simulate(deltaTime) {
    for (let index = 0; index < 4; index += 1) currentRect[index] = ease(currentRect[index], targetRect[index], deltaTime);
    for (let index = 0; index < 3; index += 1) currentTint[index] = ease(currentTint[index], targetTint[index], deltaTime);
    currentIntensity = ease(currentIntensity, targetIntensity, deltaTime);
    currentSpeed = ease(currentSpeed, targetSpeed, deltaTime);
    currentRefraction = ease(currentRefraction, targetRefraction, deltaTime);
    currentBlur = ease(currentBlur, targetBlur, deltaTime);
    currentCornerRadius = ease(currentCornerRadius, targetCornerRadius, deltaTime);
  }

  function render(frame) {
    if (disposed || !active) return;
    gl.useProgram(binding.program);
    context.bindEngineGlobals(binding, frame);
    if (locations.rect !== null) gl.uniform4f(locations.rect, currentRect[0], currentRect[1], currentRect[2], currentRect[3]);
    if (locations.tint !== null) gl.uniform3f(locations.tint, currentTint[0], currentTint[1], currentTint[2]);
    if (locations.intensity !== null) gl.uniform1f(locations.intensity, currentIntensity);
    if (locations.speed !== null) gl.uniform1f(locations.speed, currentSpeed);
    if (locations.refraction !== null) gl.uniform1f(locations.refraction, currentRefraction);
    if (locations.blur !== null) gl.uniform1f(locations.blur, currentBlur);
    if (locations.cornerRadius !== null) gl.uniform1f(locations.cornerRadius, currentCornerRadius);
    if (locations.waveDensity !== null) gl.uniform1f(locations.waveDensity, profile.waveDensity);
    if (locations.opacityCap !== null) gl.uniform1f(locations.opacityCap, profile.opacityCap);
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

export function applyDomFallback(root, detail = {}) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const attribute = 'data-aurora-glass-refraction-fallback';
  const previous = root.getAttribute(attribute);
  root.setAttribute(attribute, detail.state || 'active');
  let cleaned = false;
  return () => {
    if (cleaned) return;
    cleaned = true;
    if (previous === null) root.removeAttribute(attribute);
    else root.setAttribute(attribute, previous);
  };
}
