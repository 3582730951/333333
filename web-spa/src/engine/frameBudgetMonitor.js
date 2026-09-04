import {
  DEFAULT_PHASE_BUDGETS,
  FRAME_BUDGET_MS,
  PERFORMANCE_PHASES,
  createPerformanceBudget,
} from './performanceBudget.js';

const NOOP = () => {};
const PERCENTILE = 0.95;

function nowDefault() {
  try {
    return typeof performance !== 'undefined' && typeof performance.now === 'function'
      ? performance.now()
      : Date.now();
  } catch {
    return Date.now();
  }
}

function finite(value, fallback = 0) {
  const number = Number(value);
  return Number.isFinite(number) ? number : fallback;
}

function readTime(value, clock) {
  const number = Number(value);
  return Number.isFinite(number) ? number : clock();
}

function percentile(values, count, scratch, percentileValue = PERCENTILE) {
  if (count === 0) return 0;
  scratch.set(values.subarray(0, count));
  // Small fixed windows make insertion sort cheaper and deterministic here;
  // this runs only when a diagnostic snapshot is requested.
  for (let index = 1; index < count; index += 1) {
    const value = scratch[index];
    let cursor = index - 1;
    while (cursor >= 0 && scratch[cursor] > value) {
      scratch[cursor + 1] = scratch[cursor];
      cursor -= 1;
    }
    scratch[cursor + 1] = value;
  }
  const position = Math.min(count - 1, Math.max(0, Math.ceil(count * percentileValue) - 1));
  return scratch[position];
}

function percentileRing(values, cursor, count, capacity, scratch, percentileValue = PERCENTILE) {
  if (count === 0) return 0;
  const start = count === capacity ? cursor : 0;
  for (let index = 0; index < count; index += 1) scratch[index] = values[(start + index) % capacity];
  return percentile(scratch, count, scratch, percentileValue);
}

/**
 * Fixed-capacity sliding frame monitor. Recording uses preallocated typed
 * arrays; snapshot/copyMetrics are the only diagnostic boundaries.
 */
export function createFrameBudgetMonitor({
  windowSize = 120,
  budgets = DEFAULT_PHASE_BUDGETS,
  frameBudgetMs = FRAME_BUDGET_MS,
  now = nowDefault,
  onViolation = NOOP,
} = {}) {
  const capacity = Math.max(1, Math.floor(finite(windowSize, 120)));
  const phaseIndex = Object.create(null);
  for (let index = 0; index < PERFORMANCE_PHASES.length; index += 1) phaseIndex[PERFORMANCE_PHASES[index]] = index;
  const frameTimes = new Float64Array(capacity);
  const phaseTimes = new Float64Array(capacity * PERFORMANCE_PHASES.length);
  const scratch = new Float64Array(capacity);
  const latest = new Float64Array(PERFORMANCE_PHASES.length);
  const measurement = Object.create(null);
  const budget = createPerformanceBudget({ budgets, frameBudgetMs });
  let cursor = 0;
  let count = 0;
  let frameStart = 0;
  let phaseStart = 0;
  let activePhase = -1;
  let active = false;
  let frameCount = 0;
  let violationCount = 0;
  let lastResult = null;

  function startFrame(timestamp = now()) {
    frameStart = readTime(timestamp, now);
    latest.fill(0);
    activePhase = -1;
    active = true;
    return frameStart;
  }

  function startPhase(name, timestamp = now()) {
    const index = phaseIndex[name];
    if (index === undefined || !active) return false;
    activePhase = index;
    phaseStart = readTime(timestamp, now);
    return true;
  }

  function endPhase(timestamp = now()) {
    if (activePhase < 0 || !active) return 0;
    const duration = Math.max(0, readTime(timestamp, now) - phaseStart);
    latest[activePhase] += duration;
    activePhase = -1;
    return duration;
  }

  function recordPhase(name, durationMs) {
    const index = phaseIndex[name];
    if (index === undefined) return false;
    latest[index] = Math.max(0, finite(durationMs));
    return true;
  }

  function recordFrame(measurements = {}, totalMs) {
    for (let index = 0; index < PERFORMANCE_PHASES.length; index += 1) {
      const phase = PERFORMANCE_PHASES[index];
      latest[index] = Math.max(0, finite(measurements?.[phase], latest[index]));
      phaseTimes[cursor * PERFORMANCE_PHASES.length + index] = latest[index];
    }
    const total = Math.max(0, finite(totalMs, latest[0] + latest[1] + latest[2] + latest[3]));
    frameTimes[cursor] = total;
    measurement.input = latest[0];
    measurement.layout = latest[1];
    measurement.instructions = latest[2];
    measurement.gpu = latest[3];
    lastResult = budget.check(measurement);
    frameCount += 1;
    if (!lastResult.withinBudget) {
      violationCount += 1;
      onViolation(lastResult);
    }
    cursor = (cursor + 1) % capacity;
    count = Math.min(capacity, count + 1);
    return lastResult.withinBudget;
  }

  function endFrame(timestamp = now()) {
    if (!active) return false;
    if (activePhase >= 0) endPhase(timestamp);
    const total = Math.max(0, readTime(timestamp, now) - frameStart);
    active = false;
    return recordFrame(null, total);
  }

  function observe(measurements = {}) {
    latest[0] = Math.max(0, finite(measurements.input, 0));
    latest[1] = Math.max(0, finite(measurements.layout, 0));
    latest[2] = Math.max(0, finite(measurements.instructions, 0));
    latest[3] = Math.max(0, finite(measurements.gpu, 0));
    return recordFrame(measurements, measurements.totalMs);
  }

  function snapshot() {
    const phaseP95 = Object.create(null);
    for (let phase = 0; phase < PERFORMANCE_PHASES.length; phase += 1) {
      for (let index = 0; index < count; index += 1) {
        const ringIndex = (count === capacity ? cursor + index : index) % capacity;
        scratch[index] = phaseTimes[ringIndex * PERFORMANCE_PHASES.length + phase];
      }
      phaseP95[PERFORMANCE_PHASES[phase]] = percentile(scratch, count, scratch);
    }
    const frameP95 = percentileRing(frameTimes, cursor, count, capacity, scratch);
    return {
      windowSize: capacity,
      samples: count,
      frameCount,
      violationCount,
      frameP95Ms: frameP95,
      phaseP95Ms: phaseP95,
      withinBudget: frameP95 <= frameBudgetMs && PERFORMANCE_PHASES.every((phase) => phaseP95[phase] <= budget.budgets[phase]),
      budget: budget.snapshot(),
    };
  }

  function copyMetrics(target = {}) {
    target.samples = count;
    target.frameCount = frameCount;
    target.violationCount = violationCount;
    target.frameP95Ms = percentileRing(frameTimes, cursor, count, capacity, scratch);
    target.withinBudget = target.frameP95Ms <= frameBudgetMs;
    return target;
  }

  return {
    startFrame,
    beginFrame: startFrame,
    startPhase,
    beginPhase: startPhase,
    endPhase,
    recordPhase,
    recordFrame,
    endFrame,
    observe,
    snapshot,
    copyMetrics,
    get samples() { return count; },
    get lastResult() { return lastResult; },
  };
}
