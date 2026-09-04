import { clampUnit, ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const transition = Object.freeze({
  durationMs: 150,
  durationToken: '--pool-motion-fast',
  easing: 'cubic-bezier(.2, .8, .2, 1)',
  easingToken: '--pool-ease-standard',
  reducedMotion: 'skip-to-end',
});

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'blinds',
  title: 'Blinds transition',
  composition: Object.freeze({ slot: 'interaction', blend: 'alpha', zIndex: 42, priority: 42, exclusiveGroup: 'route-transition' }),
  transition,
  uniforms: Object.freeze({
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, description: 'Slat reveal progress.' }),
    uOrientation: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, description: '0 horizontal slats, 1 vertical slats.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.3, 0.64, 0.9], description: 'Slat edge tint.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.28, min: 0, max: 0.6, description: 'Slat opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 1, min: 0, max: 2, description: 'Subtle slat shimmer speed.' }),
    uDensity: Object.freeze({ type: 'float', default: 0.6, min: 0.2, max: 1, description: 'Number of slats.' }),
    uScale: Object.freeze({ type: 'float', default: 1, min: 0.65, max: 1.7, description: 'Slat width scale.' }),
    uSoftness: Object.freeze({ type: 'float', default: 0.09, min: 0.02, max: 0.22, description: 'Reveal edge softness.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ slatFactor: 1, alphaCap: 1 }),
    medium: Object.freeze({ slatFactor: 0.72, alphaCap: 0.78 }),
    low: Object.freeze({ slatFactor: 0.48, alphaCap: 0.54 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 0.95, medium: 0.62, low: 0.3 }),
    gpuMilliseconds: Object.freeze({ high: 0.2, medium: 0.13, low: 0.07 }),
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

function smooth(current, target, deltaSeconds) {
  const amount = 1 - Math.pow(0.001, Math.max(0, deltaSeconds) / (transition.durationMs / 1000));
  return current + (target - current) * amount;
}

function prefersReducedMotion() {
  return typeof globalThis.matchMedia === 'function'
    && globalThis.matchMedia('(prefers-reduced-motion: reduce)').matches;
}

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl;
  const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({
    progress: gl.getUniformLocation(binding.program, 'uProgress'), orientation: gl.getUniformLocation(binding.program, 'uOrientation'),
    tint: gl.getUniformLocation(binding.program, 'uTint'), intensity: gl.getUniformLocation(binding.program, 'uIntensity'),
    speed: gl.getUniformLocation(binding.program, 'uSpeed'), density: gl.getUniformLocation(binding.program, 'uDensity'),
    scale: gl.getUniformLocation(binding.program, 'uScale'), softness: gl.getUniformLocation(binding.program, 'uSoftness'),
  });
  const definitions = manifest.uniforms;
  const color = (value, fallback) => {
    const source = Array.isArray(value) || ArrayBuffer.isView(value) ? value : fallback;
    return [clampUnit(source[0]), clampUnit(source[1]), clampUnit(source[2])];
  };
  let targetProgress = bounded(initialParameters.uProgress, definitions.uProgress); let progress = targetProgress;
  let targetOrientation = bounded(initialParameters.uOrientation, definitions.uOrientation); let orientation = targetOrientation;
  let targetTint = color(initialParameters.uTint, definitions.uTint.default); let tint = targetTint.slice();
  let targetIntensity = bounded(initialParameters.uIntensity, definitions.uIntensity); let intensity = targetIntensity;
  let targetSpeed = bounded(initialParameters.uSpeed, definitions.uSpeed); let speed = targetSpeed;
  let targetDensity = bounded(initialParameters.uDensity, definitions.uDensity); let density = targetDensity;
  let targetScale = bounded(initialParameters.uScale, definitions.uScale); let scale = targetScale;
  let targetSoftness = bounded(initialParameters.uSoftness, definitions.uSoftness); let softness = targetSoftness;
  let profile = manifest.quality.medium;
  const reducedMotion = prefersReducedMotion();
  let active = !reducedMotion; let disposed = false;

  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uProgress')) targetProgress = bounded(next.uProgress, definitions.uProgress);
    if (Object.prototype.hasOwnProperty.call(next, 'uOrientation')) targetOrientation = bounded(next.uOrientation, definitions.uOrientation);
    if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = color(next.uTint, targetTint);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = bounded(next.uIntensity, definitions.uIntensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = bounded(next.uSpeed, definitions.uSpeed);
    if (Object.prototype.hasOwnProperty.call(next, 'uDensity')) targetDensity = bounded(next.uDensity, definitions.uDensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uScale')) targetScale = bounded(next.uScale, definitions.uScale);
    if (Object.prototype.hasOwnProperty.call(next, 'uSoftness')) targetSoftness = bounded(next.uSoftness, definitions.uSoftness);
  }

  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted) && !reducedMotion; }
  function resize() {}
  function simulate(deltaSeconds) {
    if (reducedMotion) { progress = 1; return; }
    progress = smooth(progress, targetProgress, deltaSeconds); orientation = smooth(orientation, targetOrientation, deltaSeconds);
    intensity = smooth(intensity, targetIntensity, deltaSeconds); speed = smooth(speed, targetSpeed, deltaSeconds);
    density = smooth(density, targetDensity, deltaSeconds); scale = smooth(scale, targetScale, deltaSeconds); softness = smooth(softness, targetSoftness, deltaSeconds);
    for (let index = 0; index < 3; index += 1) tint[index] = smooth(tint[index], targetTint[index], deltaSeconds);
  }
  function render(frame) {
    if (disposed || !active || progress <= 0.001 || progress >= 0.999) return;
    gl.useProgram(binding.program); context.bindEngineGlobals(binding, frame);
    if (locations.progress !== null) gl.uniform1f(locations.progress, clampUnit(progress));
    if (locations.orientation !== null) gl.uniform1f(locations.orientation, orientation);
    if (locations.tint !== null) gl.uniform3fv(locations.tint, tint);
    if (locations.intensity !== null) gl.uniform1f(locations.intensity, intensity * profile.alphaCap);
    if (locations.speed !== null) gl.uniform1f(locations.speed, speed);
    if (locations.density !== null) gl.uniform1f(locations.density, density * profile.slatFactor);
    if (locations.scale !== null) gl.uniform1f(locations.scale, scale);
    if (locations.softness !== null) gl.uniform1f(locations.softness, softness);
    gl.bindVertexArray(context.fullscreenVao); gl.drawArrays(gl.TRIANGLES, 0, 3);
  }
  function dispose() { if (disposed) return; disposed = true; context.disposeProgram(binding); }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const attribute = 'data-aurora-blinds-fallback'; const reasonAttribute = 'data-aurora-blinds-reason';
  const previous = root.getAttribute(attribute); const previousReason = root.getAttribute(reasonAttribute);
  root.setAttribute(attribute, 'true'); root.setAttribute(reasonAttribute, String(detail.reason || 'transition-skipped'));
  let cleaned = false;
  return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); if (previousReason === null) root.removeAttribute(reasonAttribute); else root.setAttribute(reasonAttribute, previousReason); };
}
