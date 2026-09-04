import { clampUnit, ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const transition = Object.freeze({
  durationMs: 150,
  durationToken: '--pool-motion-fast',
  easing: 'cubic-bezier(0, 0, .2, 1)',
  easingToken: '--pool-ease-enter',
  reducedMotion: 'skip-to-end',
});

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'page-slide',
  title: 'Page slide transition',
  composition: Object.freeze({ slot: 'interaction', blend: 'alpha', zIndex: 47, priority: 47, exclusiveGroup: 'route-transition' }),
  transition,
  contract: Object.freeze({ domContentOwner: true, role: 'directional-edge-accent' }),
  uniforms: Object.freeze({
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, description: 'Slide progress.' }),
    uDirection: Object.freeze({ type: 'vec2', default: [1, 0], description: 'Normalized slide direction.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.38, 0.75, 0.96], description: 'Slide edge tint.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.28, min: 0, max: 0.58, description: 'Edge accent opacity.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.75, min: 0, max: 2, description: 'Edge shimmer speed.' }),
    uDensity: Object.freeze({ type: 'float', default: 0.54, min: 0.2, max: 1, description: 'Streak density.' }),
    uScale: Object.freeze({ type: 'float', default: 1, min: 0.65, max: 1.8, description: 'Streak scale.' }),
    uDistance: Object.freeze({ type: 'float', default: 0.92, min: 0.2, max: 1.6, description: 'Normalized slide travel.' }),
    uSoftness: Object.freeze({ type: 'float', default: 0.1, min: 0.02, max: 0.24, description: 'Leading edge softness.' }),
  }),
  quality: Object.freeze({ high: Object.freeze({ streakFactor: 1, alphaCap: 1 }), medium: Object.freeze({ streakFactor: 0.72, alphaCap: 0.78 }), low: Object.freeze({ streakFactor: 0.48, alphaCap: 0.52 }) }),
  cost: Object.freeze({ budgetUnits: Object.freeze({ high: 0.98, medium: 0.64, low: 0.3 }), gpuMilliseconds: Object.freeze({ high: 0.21, medium: 0.14, low: 0.075 }), fill: 'fullscreen', allocation: 'steady-state-zero-js', estimatedDrawCalls: 1, textureBytes: 0 }),
  threading: Object.freeze({ instructionGeneration: 'worker-safe', render: 'main-or-offscreen' }),
});

function number(value, definition) { const numeric = Number(value); return Number.isFinite(numeric) ? Math.min(definition.max, Math.max(definition.min, numeric)) : definition.default; }
function vector(value, fallback, length) { const source = Array.isArray(value) || ArrayBuffer.isView(value) ? value : fallback; return Array.from({ length }, (_, index) => { const numeric = Number(source[index]); return Number.isFinite(numeric) ? numeric : Number(fallback[index]); }); }
function smooth(current, target, deltaSeconds) { const amount = 1 - Math.pow(0.001, Math.max(0, deltaSeconds) / 0.15); return current + (target - current) * amount; }
function prefersReducedMotion() { return typeof globalThis.matchMedia === 'function' && globalThis.matchMedia('(prefers-reduced-motion: reduce)').matches; }

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl; const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({ progress: gl.getUniformLocation(binding.program, 'uProgress'), direction: gl.getUniformLocation(binding.program, 'uDirection'), tint: gl.getUniformLocation(binding.program, 'uTint'), intensity: gl.getUniformLocation(binding.program, 'uIntensity'), speed: gl.getUniformLocation(binding.program, 'uSpeed'), density: gl.getUniformLocation(binding.program, 'uDensity'), scale: gl.getUniformLocation(binding.program, 'uScale'), distance: gl.getUniformLocation(binding.program, 'uDistance'), softness: gl.getUniformLocation(binding.program, 'uSoftness') });
  const d = manifest.uniforms; let targetProgress = number(initialParameters.uProgress, d.uProgress); let progress = targetProgress; let targetDirection = vector(initialParameters.uDirection, d.uDirection.default, 2); let direction = targetDirection.slice(); let targetTint = vector(initialParameters.uTint, d.uTint.default, 3).map((value) => clampUnit(value)); let tint = targetTint.slice(); let targetIntensity = number(initialParameters.uIntensity, d.uIntensity); let intensity = targetIntensity; let targetSpeed = number(initialParameters.uSpeed, d.uSpeed); let speed = targetSpeed; let targetDensity = number(initialParameters.uDensity, d.uDensity); let density = targetDensity; let targetScale = number(initialParameters.uScale, d.uScale); let scale = targetScale; let targetDistance = number(initialParameters.uDistance, d.uDistance); let distance = targetDistance; let targetSoftness = number(initialParameters.uSoftness, d.uSoftness); let softness = targetSoftness;
  let profile = manifest.quality.medium; const reducedMotion = prefersReducedMotion(); let active = !reducedMotion; let disposed = false;
  function setParameters(next = {}) { if (Object.prototype.hasOwnProperty.call(next, 'uProgress')) targetProgress = number(next.uProgress, d.uProgress); if (Object.prototype.hasOwnProperty.call(next, 'uDirection')) targetDirection = vector(next.uDirection, targetDirection, 2); if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = vector(next.uTint, targetTint, 3).map((value) => clampUnit(value)); if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = number(next.uIntensity, d.uIntensity); if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = number(next.uSpeed, d.uSpeed); if (Object.prototype.hasOwnProperty.call(next, 'uDensity')) targetDensity = number(next.uDensity, d.uDensity); if (Object.prototype.hasOwnProperty.call(next, 'uScale')) targetScale = number(next.uScale, d.uScale); if (Object.prototype.hasOwnProperty.call(next, 'uDistance')) targetDistance = number(next.uDistance, d.uDistance); if (Object.prototype.hasOwnProperty.call(next, 'uSoftness')) targetSoftness = number(next.uSoftness, d.uSoftness); }
  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted) && !reducedMotion; }
  function resize() {}
  function simulate(deltaSeconds) { if (reducedMotion) { progress = 1; return; } progress = smooth(progress, targetProgress, deltaSeconds); for (let index = 0; index < 2; index += 1) direction[index] = smooth(direction[index], targetDirection[index], deltaSeconds); for (let index = 0; index < 3; index += 1) tint[index] = smooth(tint[index], targetTint[index], deltaSeconds); intensity = smooth(intensity, targetIntensity, deltaSeconds); speed = smooth(speed, targetSpeed, deltaSeconds); density = smooth(density, targetDensity, deltaSeconds); scale = smooth(scale, targetScale, deltaSeconds); distance = smooth(distance, targetDistance, deltaSeconds); softness = smooth(softness, targetSoftness, deltaSeconds); }
  function render(frame) { if (disposed || !active || progress <= 0.001 || progress >= 0.999) return; gl.useProgram(binding.program); context.bindEngineGlobals(binding, frame); if (locations.progress !== null) gl.uniform1f(locations.progress, clampUnit(progress)); if (locations.direction !== null) gl.uniform2fv(locations.direction, direction); if (locations.tint !== null) gl.uniform3fv(locations.tint, tint); if (locations.intensity !== null) gl.uniform1f(locations.intensity, intensity * profile.alphaCap); if (locations.speed !== null) gl.uniform1f(locations.speed, speed); if (locations.density !== null) gl.uniform1f(locations.density, density * profile.streakFactor); if (locations.scale !== null) gl.uniform1f(locations.scale, scale); if (locations.distance !== null) gl.uniform1f(locations.distance, distance); if (locations.softness !== null) gl.uniform1f(locations.softness, softness); gl.bindVertexArray(context.fullscreenVao); gl.drawArrays(gl.TRIANGLES, 0, 3); }
  function dispose() { if (disposed) return; disposed = true; context.disposeProgram(binding); }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) { if (!root || typeof root.setAttribute !== 'function') return () => {}; const attribute = 'data-aurora-page-slide-fallback'; const previous = root.getAttribute(attribute); root.setAttribute(attribute, String(detail.reason || 'dom-page-slide')); let cleaned = false; return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); }; }
