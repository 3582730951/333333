import { normalizeQuality } from './contracts.js';

export const QUALITY_BUDGET_UNITS = Object.freeze({
  high: 12,
  medium: 8,
  low: 4,
});

const SLOT_LIMITS = Object.freeze({
  high: Object.freeze({ base: 1, ambient: 1, interaction: 1, foreground: 1 }),
  medium: Object.freeze({ base: 1, ambient: 1, interaction: 1, foreground: 1 }),
  low: Object.freeze({ base: 0, ambient: 1, interaction: 1, foreground: 0 }),
});

function effectUnits(entry, quality) {
  const value = Number(entry.manifest.cost?.budgetUnits?.[quality]);
  return Number.isFinite(value) ? Math.max(0, value) : Number.POSITIVE_INFINITY;
}

function compareEntries(left, right) {
  const priorityDelta = Number(right.manifest.composition.priority || 0) - Number(left.manifest.composition.priority || 0);
  if (priorityDelta !== 0) return priorityDelta;
  const zDelta = Number(left.manifest.composition.zIndex || 0) - Number(right.manifest.composition.zIndex || 0);
  if (zDelta !== 0) return zDelta;
  return left.manifest.id.localeCompare(right.manifest.id);
}

/**
 * Admission is lifecycle-time work. Render visits the already reconciled array
 * with indexed loops only, preventing both visual fights and cost explosions.
 */
export function createEffectCompositor({ gl, quality = 'medium', baseCostUnits = 0 } = {}) {
  const entries = [];
  let currentQuality = normalizeQuality(quality);
  let baseUnits = Number(baseCostUnits) || 0;
  let activeUnits = baseUnits;

  function reconcile() {
    entries.sort(compareEntries);
    const limits = SLOT_LIMITS[currentQuality];
    const occupied = { base: 0, ambient: 0, interaction: 0, foreground: 0 };
    const occupiedGroups = Object.create(null);
    let units = Math.max(0, baseUnits);
    for (let index = 0; index < entries.length; index += 1) {
      const entry = entries[index];
      const slot = entry.manifest.composition.slot;
      const unitCost = effectUnits(entry, currentQuality);
      const canFitSlot = occupied[slot] < limits[slot];
      const canFitBudget = units + unitCost <= QUALITY_BUDGET_UNITS[currentQuality];
      const group = entry.manifest.composition.exclusiveGroup;
      const canFitGroup = !group || !occupiedGroups[group];
      entry.active = Boolean(canFitSlot && canFitBudget && canFitGroup);
      if (entry.active) {
        occupied[slot] += 1;
        units += unitCost;
        if (group) occupiedGroups[group] = true;
      }
      if (typeof entry.instance.setQuality === 'function') entry.instance.setQuality(currentQuality, entry.active);
    }
    activeUnits = units;
  }

  function add(manifest, instance) {
    const entry = { manifest, instance, active: false };
    entries.push(entry);
    reconcile();
    return entry.active;
  }

  function remove(id) {
    for (let index = 0; index < entries.length; index += 1) {
      if (entries[index].manifest.id !== id) continue;
      const [entry] = entries.splice(index, 1);
      if (typeof entry.instance.dispose === 'function') entry.instance.dispose();
      reconcile();
      return true;
    }
    return false;
  }

  function setParameters(id, parameters) {
    for (let index = 0; index < entries.length; index += 1) {
      const entry = entries[index];
      if (entry.manifest.id !== id) continue;
      entry.instance.setParameters(parameters);
      return true;
    }
    return false;
  }

  function simulate(deltaTime, frame) {
    for (let index = 0; index < entries.length; index += 1) {
      const entry = entries[index];
      if (entry.active && typeof entry.instance.simulate === 'function') entry.instance.simulate(deltaTime, frame);
    }
  }

  function resize(width, height, pixelRatio) {
    for (let index = 0; index < entries.length; index += 1) {
      const instance = entries[index].instance;
      if (typeof instance.resize === 'function') instance.resize(width, height, pixelRatio);
    }
  }

  function render(frame) {
    for (let index = 0; index < entries.length; index += 1) {
      const entry = entries[index];
      if (!entry.active || typeof entry.instance.render !== 'function') continue;
      if (entry.manifest.composition.blend === 'additive') gl.blendFunc(gl.SRC_ALPHA, gl.ONE);
      else gl.blendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA);
      entry.instance.render(frame);
    }
  }

  function setQuality(nextQuality) {
    currentQuality = normalizeQuality(nextQuality);
    reconcile();
  }

  function setBaseCostUnits(nextBaseUnits) {
    baseUnits = Math.max(0, Number(nextBaseUnits) || 0);
    reconcile();
  }

  function dispose() {
    for (let index = 0; index < entries.length; index += 1) {
      const instance = entries[index].instance;
      if (typeof instance.dispose === 'function') instance.dispose();
    }
    entries.length = 0;
  }

  function copyMetrics(target) {
    target.activeUnits = activeUnits;
    target.effectCount = entries.length;
    target.quality = currentQuality;
    return target;
  }

  return {
    add,
    remove,
    setParameters,
    simulate,
    resize,
    render,
    setQuality,
    setBaseCostUnits,
    dispose,
    copyMetrics,
    get quality() { return currentQuality; },
  };
}
