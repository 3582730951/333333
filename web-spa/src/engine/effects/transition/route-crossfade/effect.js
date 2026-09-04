import { clampUnit, ENGINE_EFFECT_SCHEMA_VERSION } from '../../../contracts.js';
import { FRAGMENT_SOURCE, VERTEX_SOURCE } from './shader.js';

export const transition = Object.freeze({
  durationMs: 200,
  durationToken: '--pool-motion-normal',
  easing: 'cubic-bezier(.2, .8, .2, 1)',
  easingToken: '--pool-ease-standard',
  reducedMotion: 'skip-to-end',
});

export const manifest = Object.freeze({
  schemaVersion: ENGINE_EFFECT_SCHEMA_VERSION,
  id: 'route-crossfade',
  title: 'Route crossfade',
  composition: Object.freeze({ slot: 'interaction', blend: 'alpha', zIndex: 44, priority: 44, exclusiveGroup: 'route-transition' }),
  transition,
  contract: Object.freeze({ contentCrossfadeOwner: 'DOM-route-coordinator', shaderRole: 'palette-veil' }),
  uniforms: Object.freeze({
    uProgress: Object.freeze({ type: 'float', default: 0, min: 0, max: 1, description: 'Route transition progress.' }),
    uFromColor: Object.freeze({ type: 'vec3', default: [0.08, 0.16, 0.28], description: 'Previous route palette sample.' }),
    uToColor: Object.freeze({ type: 'vec3', default: [0.16, 0.4, 0.58], description: 'Next route palette sample.' }),
    uTint: Object.freeze({ type: 'vec3', default: [0.42, 0.78, 0.96], description: 'Crossfade glow.' }),
    uIntensity: Object.freeze({ type: 'float', default: 0.22, min: 0, max: 0.48, description: 'Veil opacity; DOM remains readable above it.' }),
    uSpeed: Object.freeze({ type: 'float', default: 0.5, min: 0, max: 2, description: 'Subtle veil motion.' }),
    uDensity: Object.freeze({ type: 'float', default: 0.46, min: 0.2, max: 1, description: 'Veil grain density.' }),
    uScale: Object.freeze({ type: 'float', default: 1, min: 0.65, max: 1.8, description: 'Veil scale.' }),
  }),
  quality: Object.freeze({ high: Object.freeze({ grainFactor: 1, alphaCap: 1 }), medium: Object.freeze({ grainFactor: 0.7, alphaCap: 0.78 }), low: Object.freeze({ grainFactor: 0.45, alphaCap: 0.52 }) }),
  cost: Object.freeze({ budgetUnits: Object.freeze({ high: 0.92, medium: 0.6, low: 0.28 }), gpuMilliseconds: Object.freeze({ high: 0.2, medium: 0.13, low: 0.07 }), fill: 'fullscreen', allocation: 'steady-state-zero-js', estimatedDrawCalls: 1, textureBytes: 0 }),
  threading: Object.freeze({ instructionGeneration: 'worker-safe', render: 'main-or-offscreen' }),
});

function number(value, definition) { const numeric = Number(value); return Number.isFinite(numeric) ? Math.min(definition.max, Math.max(definition.min, numeric)) : definition.default; }
function colour(value, fallback) { const source = Array.isArray(value) || ArrayBuffer.isView(value) ? value : fallback; return [0, 1, 2].map((index) => clampUnit(source[index], fallback[index])); }
function smooth(current, target, deltaSeconds) { const amount = 1 - Math.pow(0.001, Math.max(0, deltaSeconds) / 0.2); return current + (target - current) * amount; }
function prefersReducedMotion() { return typeof globalThis.matchMedia === 'function' && globalThis.matchMedia('(prefers-reduced-motion: reduce)').matches; }

export function createEffect(context, initialParameters = {}) {
  const gl = context.gl; const binding = context.createProgram({ vertexSource: VERTEX_SOURCE, fragmentSource: FRAGMENT_SOURCE, label: manifest.id });
  const locations = Object.freeze({ progress: gl.getUniformLocation(binding.program, 'uProgress'), fromColor: gl.getUniformLocation(binding.program, 'uFromColor'), toColor: gl.getUniformLocation(binding.program, 'uToColor'), tint: gl.getUniformLocation(binding.program, 'uTint'), intensity: gl.getUniformLocation(binding.program, 'uIntensity'), speed: gl.getUniformLocation(binding.program, 'uSpeed'), density: gl.getUniformLocation(binding.program, 'uDensity'), scale: gl.getUniformLocation(binding.program, 'uScale') });
  const d = manifest.uniforms; let targetProgress = number(initialParameters.uProgress, d.uProgress); let progress = targetProgress;
  let targetFrom = colour(initialParameters.uFromColor, d.uFromColor.default); let fromColor = targetFrom.slice(); let targetTo = colour(initialParameters.uToColor, d.uToColor.default); let toColor = targetTo.slice(); let targetTint = colour(initialParameters.uTint, d.uTint.default); let tint = targetTint.slice();
  let targetIntensity = number(initialParameters.uIntensity, d.uIntensity); let intensity = targetIntensity; let targetSpeed = number(initialParameters.uSpeed, d.uSpeed); let speed = targetSpeed; let targetDensity = number(initialParameters.uDensity, d.uDensity); let density = targetDensity; let targetScale = number(initialParameters.uScale, d.uScale); let scale = targetScale;
  let profile = manifest.quality.medium; const reducedMotion = prefersReducedMotion(); let active = !reducedMotion; let disposed = false;
  function setParameters(next = {}) { if (Object.prototype.hasOwnProperty.call(next, 'uProgress')) targetProgress = number(next.uProgress, d.uProgress); if (Object.prototype.hasOwnProperty.call(next, 'uFromColor')) targetFrom = colour(next.uFromColor, targetFrom); if (Object.prototype.hasOwnProperty.call(next, 'uToColor')) targetTo = colour(next.uToColor, targetTo); if (Object.prototype.hasOwnProperty.call(next, 'uTint')) targetTint = colour(next.uTint, targetTint); if (Object.prototype.hasOwnProperty.call(next, 'uIntensity')) targetIntensity = number(next.uIntensity, d.uIntensity); if (Object.prototype.hasOwnProperty.call(next, 'uSpeed')) targetSpeed = number(next.uSpeed, d.uSpeed); if (Object.prototype.hasOwnProperty.call(next, 'uDensity')) targetDensity = number(next.uDensity, d.uDensity); if (Object.prototype.hasOwnProperty.call(next, 'uScale')) targetScale = number(next.uScale, d.uScale); }
  function setQuality(quality, admitted) { profile = manifest.quality[quality] || manifest.quality.medium; active = Boolean(admitted) && !reducedMotion; }
  function resize() {}
  function simulate(deltaSeconds) { if (reducedMotion) { progress = 1; return; } progress = smooth(progress, targetProgress, deltaSeconds); for (let index = 0; index < 3; index += 1) { fromColor[index] = smooth(fromColor[index], targetFrom[index], deltaSeconds); toColor[index] = smooth(toColor[index], targetTo[index], deltaSeconds); tint[index] = smooth(tint[index], targetTint[index], deltaSeconds); } intensity = smooth(intensity, targetIntensity, deltaSeconds); speed = smooth(speed, targetSpeed, deltaSeconds); density = smooth(density, targetDensity, deltaSeconds); scale = smooth(scale, targetScale, deltaSeconds); }
  function render(frame) { if (disposed || !active || progress <= 0.001 || progress >= 0.999) return; gl.useProgram(binding.program); context.bindEngineGlobals(binding, frame); if (locations.progress !== null) gl.uniform1f(locations.progress, clampUnit(progress)); if (locations.fromColor !== null) gl.uniform3fv(locations.fromColor, fromColor); if (locations.toColor !== null) gl.uniform3fv(locations.toColor, toColor); if (locations.tint !== null) gl.uniform3fv(locations.tint, tint); if (locations.intensity !== null) gl.uniform1f(locations.intensity, intensity * profile.alphaCap); if (locations.speed !== null) gl.uniform1f(locations.speed, speed); if (locations.density !== null) gl.uniform1f(locations.density, density * profile.grainFactor); if (locations.scale !== null) gl.uniform1f(locations.scale, scale); gl.bindVertexArray(context.fullscreenVao); gl.drawArrays(gl.TRIANGLES, 0, 3); }
  function dispose() { if (disposed) return; disposed = true; context.disposeProgram(binding); }
  return { setParameters, setQuality, resize, simulate, render, dispose };
}

export function applyDomFallback(root, detail = {}) { if (!root || typeof root.setAttribute !== 'function') return () => {}; const attribute = 'data-aurora-route-crossfade-fallback'; const previous = root.getAttribute(attribute); root.setAttribute(attribute, String(detail.reason || 'dom-crossfade')); let cleaned = false; return () => { if (cleaned) return; cleaned = true; if (previous === null) root.removeAttribute(attribute); else root.setAttribute(attribute, previous); }; }
