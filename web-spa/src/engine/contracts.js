/**
 * Aurora effect contract shared by every effect directory.
 *
 * This module deliberately has no browser side effects.  It is imported only by
 * the lazy engine entry, never by the initial application graph.
 */

export const ENGINE_EFFECT_SCHEMA_VERSION = 1;
export const QUALITY_LEVELS = Object.freeze(['high', 'medium', 'low']);
export const COMPOSITION_SLOTS = Object.freeze(['base', 'ambient', 'interaction', 'foreground']);
export const BLEND_MODES = Object.freeze(['alpha', 'additive']);
const ENGINE_GLOBAL_UNIFORM_NAMES = new Set([
  'uTime', 'uDeltaTime', 'uResolution', 'uPixelRatio', 'uQuality',
  'uAtmoVoid', 'uAtmoNear', 'uAtmoFar', 'uAtmoGlow',
]);

export function normalizeQuality(value) {
  return QUALITY_LEVELS.includes(value) ? value : 'medium';
}

export function clampUnit(value, fallback = 0) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric)) return fallback;
  return Math.min(1, Math.max(0, numeric));
}

/**
 * Validates the parts of an effect module which are safe to validate before its
 * shader creates GPU resources.  Validation runs only during lazy loading, so
 * its small object allocations never enter the frame path.
 */
export function validateEffectModule(module) {
  const errors = [];
  const manifest = module?.manifest;
  if (!manifest || typeof manifest !== 'object') errors.push('missing manifest export');
  if (typeof module?.createEffect !== 'function') errors.push('missing createEffect export');
  if (typeof module?.applyDomFallback !== 'function') errors.push('missing applyDomFallback export');
  if (!manifest || typeof manifest !== 'object') return { valid: false, errors };

  if (manifest.schemaVersion !== ENGINE_EFFECT_SCHEMA_VERSION) {
    errors.push(`unsupported schemaVersion: ${String(manifest.schemaVersion)}`);
  }
  if (!/^[a-z0-9]+(?:-[a-z0-9]+)*$/.test(String(manifest.id || ''))) {
    errors.push('manifest.id must be lowercase kebab-case');
  }
  if (!manifest.composition || !COMPOSITION_SLOTS.includes(manifest.composition.slot)) {
    errors.push('manifest.composition.slot is invalid');
  }
  if (!BLEND_MODES.includes(manifest.composition?.blend)) {
    errors.push('manifest.composition.blend is invalid');
  }
  for (let index = 0; index < QUALITY_LEVELS.length; index += 1) {
    const quality = QUALITY_LEVELS[index];
    if (!manifest.quality || !manifest.quality[quality]) {
      errors.push(`missing quality.${quality}`);
    }
    if (!manifest.cost?.budgetUnits || !Number.isFinite(manifest.cost.budgetUnits[quality])) {
      errors.push(`missing cost.budgetUnits.${quality}`);
    }
  }
  if (!manifest.uniforms || typeof manifest.uniforms !== 'object') {
    errors.push('missing uniforms table');
  } else {
    const uniformNames = Object.keys(manifest.uniforms);
    for (let index = 0; index < uniformNames.length; index += 1) {
      const name = uniformNames[index];
      const definition = manifest.uniforms[name];
      if (ENGINE_GLOBAL_UNIFORM_NAMES.has(name)) errors.push(`uniform ${name} is engine-owned`);
      if (!definition || typeof definition !== 'object' || typeof definition.type !== 'string') {
        errors.push(`uniform ${name} lacks a type`);
      }
      if (!definition || !Object.prototype.hasOwnProperty.call(definition, 'default')) {
        errors.push(`uniform ${name} lacks a default`);
      }
    }
  }
  if (!manifest.threading || !['main-only', 'worker-safe'].includes(manifest.threading.instructionGeneration)) {
    errors.push('manifest.threading.instructionGeneration is invalid');
  }
  if (!manifest.threading || !['main-only', 'main-or-offscreen'].includes(manifest.threading.render)) {
    errors.push('manifest.threading.render is invalid');
  }

  return { valid: errors.length === 0, errors };
}

/**
 * Copies caller-owned effect parameters at a lifecycle boundary.  Parameters
 * are intentionally not copied during render or simulation.
 */
export function copyEffectParameters(parameters = {}) {
  const copy = Object.create(null);
  const names = Object.keys(parameters);
  for (let index = 0; index < names.length; index += 1) {
    const name = names[index];
    const value = parameters[name];
    copy[name] = Array.isArray(value) ? value.slice() : value;
  }
  return copy;
}
