import { clampUnit, ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const transition = Object.freeze({
  durationMs: 300,
  durationToken: '--pool-motion-slow',
  easing: 'cubic-bezier(0, 0, .2, 1)',
  easingToken: '--pool-ease-enter',
  reducedMotion: 'skip-to-end',
});

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'modal-expand',
  title: 'Modal expand transition',
  composition: Object.freeze({ slot: 'foreground', blend: 'alpha', zIndex: 46, priority: 46, exclusiveGroup: 'route-transition' }),
  transition,
  contract: Object.freeze({ domContentOwner: true, role: 'non-opaque-modal-edge-underlay' }),
  uniforms: Object.freeze({
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, description: 'Expansion progress.' }),
    uOrigin: Object.freeze({ type: 'vec2', default: [0.5, 0.5], description: 'Normalized trigger origin.' }),
    uFromRect: Object.freeze({ type: 'vec4', default: [0.02, 0.02, 0.02, 0.02], description: 'Collapsed rect.' }),
    uToRect: Object.freeze({ type: 'vec4', default: [0.2, 0.16, 0.6, 0.68], description: 'Expanded modal rect.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.48, 0.82, 1], description: 'Modal edge tint.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.3, min: 0, max: 0.62, description: 'Edge opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.45, min: 0, max: 2, description: 'Edge shimmer speed.' }),
    uDensity: Object.freeze({ type: 'float', default: 0.5, min: 0.2, max: 1, description: 'Edge detail density.' }),
    uScale: Object.freeze({ type: 'float', default: 1, min: 0.7, max: 1.4, description: 'Edge scale.' }),
    uCornerRadius: Object.freeze({ type: 'float', default: 0.12, min: 0, max: 0.45, description: 'Corner radius.' }),
  }),
  quality: Object.freeze({ high: Object.freeze({ edgeFactor: 1, alphaCap: 1 }), medium: Object.freeze({ edgeFactor: 0.72, alphaCap: 0.78 }), low: Object.freeze({ edgeFactor: 0.46, alphaCap: 0.52 }) }),
  cost: Object.freeze({ budgetUnits: Object.freeze({ high: 1.2, medium: 0.78, low: 0.38 }), gpuMilliseconds: Object.freeze({ high: 0.26, medium: 0.17, low: 0.09 }), fill: 'partial', allocation: 'event-only-js', estimatedDrawCalls: 1, textureBytes: 0 }),
  threading: Object.freeze({ instructionGeneration: 'main-only', render: 'main-or-offscreen' }),
});

function number(value, definition) { const numeric = Number(value); return Number.isFinite(numeric) ? Math.min(definition.max, Math.max(definition.min, numeric)) : definition.default; }
function vector(value, fallback, length, minimum = 0, maximum = 1) { const source = Array.isArray(value) || ArrayBuffer.isView(value) ? value : fallback; return Array.from({ length }, (_, index) => { const numeric = Number(source[index]); return Number.isFinite(numeric) ? Math.min(maximum, Math.max(minimum, numeric)) : Number(fallback[index]); }); }
function rect(value, fallback) { return vector(value, fallback, 4, -1, 2).map((component, index) => index < 2 ? component : Math.max(0.001, component)); }
function smooth(current, target, deltaSeconds) { const amount = 1 - Math.pow(0.001, Math.max(0, deltaSeconds) / 0.3); return current + (target - current) * amount; }
function prefersReducedMotion() { return typeof globalThis.matchMedia === 'function' && globalThis.matchMedia('(prefers-reduced-motion: reduce)').matches; }

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl; const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({ progress: gl.getUniformLocation(binding.program, 'uProgress'), origin: gl.getUniformLocation(binding.program, 'uOrigin'), fromRect: gl.getUniformLocation(binding.program, 'uFromRect'), toRect: gl.getUniformLocation(binding.program, 'uToRect'), tint: gl.getUniformLocation(binding.program, 'uTint'), intensity: gl.getUniformLocation(binding.program, 'uIntensity'), speed: gl.getUniformLocation(binding.program, 'uSpeed'), density: gl.getUniformLocation(binding.program, 'uDensity'), scale: gl.getUniformLocation(binding.program, 'uScale'), cornerRadius: gl.getUniformLocation(binding.program, 'uCornerRadius') });
  const d = manifest.uniforms; let targetProgress = number(initialParameters.uProgress, d.uProgress); let progress = targetProgress; let targetOrigin = vector(initialParameters.uOrigin, d.uOrigin.default, 2); let origin = targetOrigin.slice(); let targetFrom = rect(initialParameters.uFromRect, d.uFromRect.default); let fromRect = targetFrom.slice(); let targetTo = rect(initialParameters.uToRect, d.uToRect.default); let toRect = targetTo.slice(); let targetTint = vector(initialParameters.uTint, d.uTint.default, 3).map((value) => clampUnit(value)); let tint = targetTint.slice(); let targetIntensity = number(initialParameters.uIntensity, d.uIntensity); let intensity = targetIntensity; let targetSpeed = number(initialParameters.uSpeed, d.uSpeed); let speed = targetSpeed; let targetDensity = number(initialParameters.uDensity, d.uDensity); let density = targetDensity; let targetScale = number(initialParameters.uScale, d.uScale); let scale = targetScale; let targetCornerRadius = number(initialParameters.uCornerRadius, d.uCornerRadius); let cornerRadius = targetCornerRadius;
  let profile = manifest.quality.medium; const reducedMotion = prefersReducedMotion(); let active = !reducedMotion; let disposed = false;
  function setParameters(next = {}) { if (Object.prototype.hasOwnProperty.call(next, 'uProgress')) targetProgress = number(next.uProgress, d.uProgress); if (Object.prototype.hasOwnProperty.call(next, 'uOrigin')) targetOrigin = vector(next.uOrigin, targetOrigin, 2); if (Object.prototype.hasOwnProperty.call(next, 'uFromRect')) targetFrom = rect(next.uFromRect, targetFrom); if (Object.prototype.hasOwnProperty.call(next, 'uToRect')) targetTo = rect(next.uToRect, targetTo); if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = vector(next.uTint, targetTint, 3).map((value) => clampUnit(value)); if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = number(next.uIntensity, d.uIntensity); if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = number(next.uSpeed, d.uSpeed); if (Object.prototype.hasOwnProperty.call(next, 'uDensity')) targetDensity = number(next.uDensity, d.uDensity); if (Object.prototype.hasOwnProperty.call(next, 'uScale')) targetScale = number(next.uScale, d.uScale); if (Object.prototype.hasOwnProperty.call(next, 'uCornerRadius')) targetCornerRadius = number(next.uCornerRadius, d.uCornerRadius); }
  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted) && !reducedMotion; }
  function resize() {}
  function simulate(deltaSeconds) { if (reducedMotion) { progress = 1; return; } progress = smooth(progress, targetProgress, deltaSeconds); for (let index = 0; index < 2; index += 1) origin[index] = smooth(origin[index], targetOrigin[index], deltaSeconds); for (let index = 0; index < 4; index += 1) { fromRect[index] = smooth(fromRect[index], targetFrom[index], deltaSeconds); toRect[index] = smooth(toRect[index], targetTo[index], deltaSeconds); } for (let index = 0; index < 3; index += 1) tint[index] = smooth(tint[index], targetTint[index], deltaSeconds); intensity = smooth(intensity, targetIntensity, deltaSeconds); speed = smooth(speed, targetSpeed, deltaSeconds); density = smooth(density, targetDensity, deltaSeconds); scale = smooth(scale, targetScale, deltaSeconds); cornerRadius = smooth(cornerRadius, targetCornerRadius, deltaSeconds); }
  function render(frame) { if (disposed || !active || progress <= 0.001 || progress >= 0.999) return; gl.useProgram(binding.program); context.bindEngineGlobals(binding, frame); if (locations.progress !== null) gl.uniform1f(locations.progress, clampUnit(progress)); if (locations.origin !== null) gl.uniform2fv(locations.origin, origin); if (locations.fromRect !== null) gl.uniform4fv(locations.fromRect, fromRect); if (locations.toRect !== null) gl.uniform4fv(locations.toRect, toRect); if (locations.tint !== null) gl.uniform3fv(locations.tint, tint); if (locations.intensity !== null) gl.uniform1f(locations.intensity, intensity * profile.alphaCap); if (locations.speed !== null) gl.uniform1f(locations.speed, speed); if (locations.density !== null) gl.uniform1f(locations.density, density * profile.edgeFactor); if (locations.scale !== null) gl.uniform1f(locations.scale, scale); if (locations.cornerRadius !== null) gl.uniform1f(locations.cornerRadius, cornerRadius); gl.bindVertexArray(context.fullscreenVao); gl.drawArrays(gl.TRIANGLES, 0, 3); }
  function dispose() { if (disposed) return; disposed = true; context.disposeProgram(binding); }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) { if (!root || typeof root.setAttribute !== 'function') return () => {}; const attribute = 'data-aurora-modal-expand-fallback'; const previous = root.getAttribute(attribute); root.setAttribute(attribute, String(detail.reason || 'dom-modal-transition')); let cleaned = false; return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); }; }
