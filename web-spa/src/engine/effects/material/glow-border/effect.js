import { ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'glow-border',
  title: 'Glow Border',
  composition: Object.freeze({
    slot: 'foreground',
    blend: 'additive',
    zIndex: 44,
    priority: 44,
    exclusiveGroup: 'material-outline',
  }),
  uniforms: Object.freeze({
    uElementRect: Object.freeze({ type: 'vec4', default: [0.25, 0.25, 0.5, 0.5], description: 'Normalized [left, bottom, width, height] target rect.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.4, 0.9, 1.0], description: 'Border color blended with the Aurora glow palette.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.52, min: 0, max: 1 }),
    uSpeed: Object.freeze({ type: 'float', default: 0.6, min: 0, max: 3 }),
    uBorderWidth: Object.freeze({ type: 'float', default: 0.018, min: 0.002, max: 0.1 }),
    uSoftness: Object.freeze({ type: 'float', default: 0.04, min: 0.002, max: 0.16 }),
    uCornerRadius: Object.freeze({ type: 'float', default: 0.11, min: 0, max: 0.3 }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ segmentDensity: 1, shineStrength: 1, alphaCap: 1 }),
    medium: Object.freeze({ segmentDensity: 0.72, shineStrength: 0.7, alphaCap: 0.78 }),
    low: Object.freeze({ segmentDensity: 0.42, shineStrength: 0, alphaCap: 0.48 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 0.45, medium: 0.28, low: 0.12 }),
    gpuMilliseconds: Object.freeze({ high: 0.11, medium: 0.07, low: 0.04 }),
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
  return [component(value, 0, fallback, -0.5, 1.5), component(value, 1, fallback, -0.5, 1.5), component(value, 2, fallback, 0.001, 2), component(value, 3, fallback, 0.001, 2)];
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
    borderWidth: gl.getUniformLocation(binding.program, 'uBorderWidth'), softness: gl.getUniformLocation(binding.program, 'uSoftness'),
    cornerRadius: gl.getUniformLocation(binding.program, 'uCornerRadius'), segmentDensity: gl.getUniformLocation(binding.program, 'uSegmentDensity'),
    shineStrength: gl.getUniformLocation(binding.program, 'uShineStrength'), alphaCap: gl.getUniformLocation(binding.program, 'uAlphaCap'),
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
  let targetBorderWidth = number(initialParameters.uBorderWidth, definitions.uBorderWidth);
  let currentBorderWidth = targetBorderWidth;
  let targetSoftness = number(initialParameters.uSoftness, definitions.uSoftness);
  let currentSoftness = targetSoftness;
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
    if (Object.prototype.hasOwnProperty.call(next, 'uBorderWidth')) targetBorderWidth = number(next.uBorderWidth, definitions.uBorderWidth);
    if (Object.prototype.hasOwnProperty.call(next, 'uSoftness')) targetSoftness = number(next.uSoftness, definitions.uSoftness);
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
    currentBorderWidth = ease(currentBorderWidth, targetBorderWidth, deltaTime);
    currentSoftness = ease(currentSoftness, targetSoftness, deltaTime);
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
    if (locations.borderWidth !== null) gl.uniform1f(locations.borderWidth, currentBorderWidth);
    if (locations.softness !== null) gl.uniform1f(locations.softness, currentSoftness);
    if (locations.cornerRadius !== null) gl.uniform1f(locations.cornerRadius, currentCornerRadius);
    if (locations.segmentDensity !== null) gl.uniform1f(locations.segmentDensity, profile.segmentDensity);
    if (locations.shineStrength !== null) gl.uniform1f(locations.shineStrength, profile.shineStrength);
    if (locations.alphaCap !== null) gl.uniform1f(locations.alphaCap, profile.alphaCap);
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
  const attribute = 'data-aurora-glow-border-fallback';
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
