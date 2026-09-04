import { QUALITY_LEVELS, normalizeQuality } from './contracts.js';
import { FRAME_BUDGET_MS } from './performanceBudget.js';

export const ADAPTIVE_QUALITY_LEVELS = Object.freeze(['extreme', 'high', 'medium', 'power-saving']);
export const DEFAULT_MEMORY_BUDGET_BYTES = 256 * 1024 * 1024;

function finite(value, fallback) {
  const number = Number(value);
  return Number.isFinite(number) ? number : fallback;
}

function engineQuality(level) {
  level = normalizeAdaptiveQuality(level) || 'high';
  if (level === 'extreme' || level === 'high') return 'high';
  if (level === 'medium') return 'medium';
  return 'low';
}

function normalizeAdaptiveQuality(level) {
  if (level === 'ultra') return 'extreme';
  if (level === '省电' || level === 'low' || level === 'power') return 'power-saving';
  return ADAPTIVE_QUALITY_LEVELS.includes(level) ? level : null;
}

function levelIndex(level) {
  const index = ADAPTIVE_QUALITY_LEVELS.indexOf(level);
  return index < 0 ? 1 : index;
}

function percentile(values, count, scratch) {
  if (!count) return 0;
  scratch.set(values.subarray(0, count));
  for (let i = 1; i < count; i += 1) {
    const value = scratch[i];
    let j = i - 1;
    while (j >= 0 && scratch[j] > value) { scratch[j + 1] = scratch[j]; j -= 1; }
    scratch[j + 1] = value;
  }
  return scratch[Math.min(count - 1, Math.ceil(count * 0.95) - 1)];
}

/** Sliding-window controller with consecutive-sample hysteresis. */
export function createAdaptiveQualityMonitor({
  initialQuality = 'high',
  windowSize = 60,
  frameBudgetMs = FRAME_BUDGET_MS,
  degradeAfter = 2,
  improveAfter = 4,
  recoveryRatio = 0.75,
  memoryBudgetBytes = DEFAULT_MEMORY_BUDGET_BYTES,
  onQualityChange,
} = {}) {
  const capacity = Math.max(1, Math.floor(finite(windowSize, 60)));
  const samples = new Float64Array(capacity);
  const memorySamples = new Float64Array(capacity);
  const scratch = new Float64Array(capacity);
  let quality = normalizeAdaptiveQuality(initialQuality) || 'high';
  let cursor = 0;
  let count = 0;
  let badStreak = 0;
  let goodStreak = 0;
  let transitions = 0;
  let lastReason = 'initial';
  let disposed = false;

  function setQuality(next, reason = 'manual') {
    if (disposed) return false;
    const normalized = normalizeAdaptiveQuality(next);
    if (!normalized) return false;
    if (normalized === quality) return false;
    const previous = quality;
    quality = normalized;
    transitions += 1;
    lastReason = reason;
    if (typeof onQualityChange === 'function') onQualityChange({ quality, previousQuality: previous, engineQuality: engineQuality(quality), reason });
    return true;
  }

  function observe(frameMs, memoryBytes = 0) {
    if (disposed) return quality;
    samples[cursor] = Math.max(0, finite(frameMs, 0));
    memorySamples[cursor] = Math.max(0, finite(memoryBytes, 0));
    cursor = (cursor + 1) % capacity;
    count = Math.min(capacity, count + 1);
    if (count < capacity) return quality;
    const p95 = percentile(samples, count, scratch);
    let memoryPeak = 0;
    for (let index = 0; index < count; index += 1) memoryPeak = Math.max(memoryPeak, memorySamples[index]);
    const overBudget = p95 > frameBudgetMs || (memoryBudgetBytes > 0 && memoryPeak > memoryBudgetBytes);
    if (overBudget) { badStreak += 1; goodStreak = 0; }
    else if (p95 <= frameBudgetMs * recoveryRatio && (memoryBudgetBytes <= 0 || memoryPeak <= memoryBudgetBytes * recoveryRatio)) { goodStreak += 1; badStreak = 0; }
    else { badStreak = 0; goodStreak = 0; }
    if (badStreak >= Math.max(1, Math.floor(degradeAfter))) {
      badStreak = 0;
      setQuality(ADAPTIVE_QUALITY_LEVELS[Math.min(ADAPTIVE_QUALITY_LEVELS.length - 1, levelIndex(quality) + 1)], 'frame-budget-overrun');
    } else if (goodStreak >= Math.max(1, Math.floor(improveAfter))) {
      goodStreak = 0;
      setQuality(ADAPTIVE_QUALITY_LEVELS[Math.max(0, levelIndex(quality) - 1)], 'sustained-headroom');
    }
    return quality;
  }

  function snapshot() {
    const p95 = percentile(samples, count, scratch);
    let memoryPeakBytes = 0;
    for (let index = 0; index < count; index += 1) memoryPeakBytes = Math.max(memoryPeakBytes, memorySamples[index]);
    return { quality, engineQuality: engineQuality(quality), samples: count, frameP95Ms: p95, memoryPeakBytes, memoryBudgetBytes, memoryPressure: memoryBudgetBytes > 0 ? memoryPeakBytes / memoryBudgetBytes : 0, badStreak, goodStreak, transitions, lastReason };
  }

  function copyMetrics(target = {}) {
    target.quality = quality;
    target.engineQuality = engineQuality(quality);
    target.samples = count;
    target.frameP95Ms = percentile(samples, count, scratch);
    let memoryPeakBytes = 0;
    for (let index = 0; index < count; index += 1) memoryPeakBytes = Math.max(memoryPeakBytes, memorySamples[index]);
    target.memoryPeakBytes = memoryPeakBytes;
    target.memoryBudgetBytes = memoryBudgetBytes;
    target.transitions = transitions;
    return target;
  }

  function reset(nextQuality = quality) {
    quality = normalizeAdaptiveQuality(nextQuality) || quality;
    cursor = 0; count = 0; badStreak = 0; goodStreak = 0; lastReason = 'reset';
  }

  return {
    observe,
    record: observe,
    setQuality,
    reset,
    snapshot,
    copyMetrics,
    dispose() { disposed = true; },
    get quality() { return quality; },
    get engineQuality() { return engineQuality(quality); },
    get disposed() { return disposed; },
    levels: ADAPTIVE_QUALITY_LEVELS,
    toEngineQuality: engineQuality,
    normalizeQuality: normalizeAdaptiveQuality,
    // Expose the P3 three-level vocabulary for callers that need to cap a
    // manual request before entering the four-level adaptive controller.
    normalizeEngineQuality: normalizeQuality,
    engineQualityLevels: QUALITY_LEVELS,
  };
}

export { engineQuality, normalizeAdaptiveQuality };
