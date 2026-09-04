import { clampUnit, ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const transition = Object.freeze({
  durationMs: 300,
  durationToken: '--pool-motion-slow',
  easing: 'cubic-bezier(.2, 0, 0, 1)',
  easingToken: '--pool-ease-emphasized',
  reducedMotion: 'skip-to-end',
});

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'shared-element-flight',
  title: 'Shared element flight',
  composition: Object.freeze({ slot: 'foreground', blend: 'alpha', zIndex: 45, priority: 45, exclusiveGroup: 'route-transition' }),
  transition,
  // P3 does not provide a cross-route geometry/texture handoff. The caller may
  // still provide normalized rectangles; without them the DOM fallback owns the flight.
  contract: Object.freeze({ requiresCrossRouteGeometry: true, geometryOwner: 'route-coordinator', domContentOwner: true }),
  uniforms: Object.freeze({
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, description: 'Flight progress.' }),
    uFromRect: Object.freeze({ type: 'vec4', default: [0.35, 0.4, 0.3, 0.16], description: 'Source [x,y,width,height] in normalized viewport coordinates.' }),
    uToRect: Object.freeze({ type: 'vec4', default: [0.18, 0.2, 0.64, 0.58], description: 'Destination [x,y,width,height] in normalized viewport coordinates.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.46, 0.82, 1], description: 'Flight outline tint.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.32, min: 0, max: 0.66, description: 'Outline opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.7, min: 0, max: 2, description: 'Trail shimmer speed.' }),
    uDensity: Object.freeze({ type: 'float', default: 0.52, min: 0.2, max: 1, description: 'Trail detail density.' }),
    uScale: Object.freeze({ type: 'float', default: 1, min: 0.7, max: 1.4, description: 'Flight outline scale.' }),
    uCornerRadius: Object.freeze({ type: 'float', default: 0.1, min: 0, max: 0.45, description: 'Normalized corner radius.' }),
    uGeometryReady: Object.freeze({ type: 'float', default: 1, min: 0, max: 1, description: '1 when route geometry was captured.' }),
  }),
  quality: Object.freeze({
    high: Object.freeze({ trailFactor: 1, alphaCap: 1 }),
    medium: Object.freeze({ trailFactor: 0.72, alphaCap: 0.78 }),
    low: Object.freeze({ trailFactor: 0.46, alphaCap: 0.52 }),
  }),
  cost: Object.freeze({
    budgetUnits: Object.freeze({ high: 1.35, medium: 0.88, low: 0.42 }),
    gpuMilliseconds: Object.freeze({ high: 0.3, medium: 0.2, low: 0.11 }),
    fill: 'partial',
    allocation: 'event-only-js',
    estimatedDrawCalls: 1,
    textureBytes: 0,
  }),
  threading: Object.freeze({ instructionGeneration: 'main-only', render: 'main-or-offscreen' }),
});

function number(value, definition) { const numeric = Number(value); return Number.isFinite(numeric) ? Math.min(definition.max, Math.max(definition.min, numeric)) : definition.default; }
function rect(value, fallback) {
  const source = Array.isArray(value) || ArrayBuffer.isView(value) ? value : fallback;
  return [0, 1, 2, 3].map((index) => { const numeric = Number(source[index]); const lower = index < 2 ? -1 : 0.001; const upper = index < 2 ? 2 : 2; return Number.isFinite(numeric) ? Math.min(upper, Math.max(lower, numeric)) : Number(fallback[index]); });
}
function tint(value, fallback) { const source = Array.isArray(value) || ArrayBuffer.isView(value) ? value : fallback; return [0, 1, 2].map((index) => clampUnit(source[index], fallback[index])); }
function smooth(current, target, deltaSeconds) { const amount = 1 - Math.pow(0.001, Math.max(0, deltaSeconds) / 0.3); return current + (target - current) * amount; }
function prefersReducedMotion() { return typeof globalThis.matchMedia === 'function' && globalThis.matchMedia('(prefers-reduced-motion: reduce)').matches; }

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl; const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({ progress: gl.getUniformLocation(binding.program, 'uProgress'), fromRect: gl.getUniformLocation(binding.program, 'uFromRect'), toRect: gl.getUniformLocation(binding.program, 'uToRect'), tint: gl.getUniformLocation(binding.program, 'uTint'), intensity: gl.getUniformLocation(binding.program, 'uIntensity'), speed: gl.getUniformLocation(binding.program, 'uSpeed'), density: gl.getUniformLocation(binding.program, 'uDensity'), scale: gl.getUniformLocation(binding.program, 'uScale'), cornerRadius: gl.getUniformLocation(binding.program, 'uCornerRadius'), geometryReady: gl.getUniformLocation(binding.program, 'uGeometryReady') });
  const d = manifest.uniforms;
  let targetProgress = number(initialParameters.uProgress, d.uProgress); let progress = targetProgress;
  let targetFrom = rect(initialParameters.uFromRect, d.uFromRect.default); let fromRect = targetFrom.slice();
  let targetTo = rect(initialParameters.uToRect, d.uToRect.default); let toRect = targetTo.slice();
  let targetTint = tint(initialParameters.uTint, d.uTint.default); let currentTint = targetTint.slice();
  let targetIntensity = number(initialParameters.uIntensity, d.uIntensity); let intensity = targetIntensity;
  let targetSpeed = number(initialParameters.uSpeed, d.uSpeed); let speed = targetSpeed;
  let targetDensity = number(initialParameters.uDensity, d.uDensity); let density = targetDensity;
  let targetScale = number(initialParameters.uScale, d.uScale); let scale = targetScale;
  let targetCornerRadius = number(initialParameters.uCornerRadius, d.uCornerRadius); let cornerRadius = targetCornerRadius;
  let targetGeometryReady = number(initialParameters.uGeometryReady, d.uGeometryReady); let geometryReady = targetGeometryReady;
  let profile = manifest.quality.medium; const reducedMotion = prefersReducedMotion(); let active = !reducedMotion; let disposed = false;
  function setParameters(next = {}) {
    if (Object.prototype.hasOwnProperty.call(next, 'uProgress')) targetProgress = number(next.uProgress, d.uProgress);
    if (Object.prototype.hasOwnProperty.call(next, 'uFromRect')) targetFrom = rect(next.uFromRect, targetFrom);
    if (Object.prototype.hasOwnProperty.call(next, 'uToRect')) targetTo = rect(next.uToRect, targetTo);
    if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = tint(next.uTint, targetTint);
    if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = number(next.uIntensity, d.uIntensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = number(next.uSpeed, d.uSpeed);
    if (Object.prototype.hasOwnProperty.call(next, 'uDensity')) targetDensity = number(next.uDensity, d.uDensity);
    if (Object.prototype.hasOwnProperty.call(next, 'uScale')) targetScale = number(next.uScale, d.uScale);
    if (Object.prototype.hasOwnProperty.call(next, 'uCornerRadius')) targetCornerRadius = number(next.uCornerRadius, d.uCornerRadius);
    if (Object.prototype.hasOwnProperty.call(next, 'uGeometryReady')) targetGeometryReady = number(next.uGeometryReady, d.uGeometryReady);
  }
  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted) && !reducedMotion; }
  function resize() {}
  function simulate(deltaSeconds) {
    if (reducedMotion) { progress = 1; return; }
    progress = smooth(progress, targetProgress, deltaSeconds); for (let index = 0; index < 4; index += 1) { fromRect[index] = smooth(fromRect[index], targetFrom[index], deltaSeconds); toRect[index] = smooth(toRect[index], targetTo[index], deltaSeconds); }
    for (let index = 0; index < 3; index += 1) currentTint[index] = smooth(currentTint[index], targetTint[index], deltaSeconds);
    intensity = smooth(intensity, targetIntensity, deltaSeconds); speed = smooth(speed, targetSpeed, deltaSeconds); density = smooth(density, targetDensity, deltaSeconds); scale = smooth(scale, targetScale, deltaSeconds); cornerRadius = smooth(cornerRadius, targetCornerRadius, deltaSeconds); geometryReady = smooth(geometryReady, targetGeometryReady, deltaSeconds);
  }
  function render(frame) {
    if (disposed || !active || progress <= 0.001 || progress >= 0.999 || geometryReady <= 0.001) return;
    gl.useProgram(binding.program); context.bindEngineGlobals(binding, frame);
    if (locations.progress !== null) gl.uniform1f(locations.progress, clampUnit(progress)); if (locations.fromRect !== null) gl.uniform4fv(locations.fromRect, fromRect); if (locations.toRect !== null) gl.uniform4fv(locations.toRect, toRect); if (locations.tint !== null) gl.uniform3fv(locations.tint, currentTint); if (locations.intensity !== null) gl.uniform1f(locations.intensity, intensity * profile.alphaCap); if (locations.speed !== null) gl.uniform1f(locations.speed, speed); if (locations.density !== null) gl.uniform1f(locations.density, density * profile.trailFactor); if (locations.scale !== null) gl.uniform1f(locations.scale, scale); if (locations.cornerRadius !== null) gl.uniform1f(locations.cornerRadius, cornerRadius); if (locations.geometryReady !== null) gl.uniform1f(locations.geometryReady, geometryReady);
    gl.bindVertexArray(context.fullscreenVao); gl.drawArrays(gl.TRIANGLES, 0, 3);
  }
  function dispose() { if (disposed) return; disposed = true; context.disposeProgram(binding); }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) {
  if (!root || typeof root.setAttribute !== 'function') return () => {};
  const attribute = 'data-aurora-shared-element-flight-fallback'; const previous = root.getAttribute(attribute); root.setAttribute(attribute, String(detail.reason || 'cross-route-geometry-unavailable')); let cleaned = false;
  return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); };
}
