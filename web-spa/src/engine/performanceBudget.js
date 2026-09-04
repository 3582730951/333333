/**
 * Runtime performance budget definitions shared by the frame monitor and the
 * adaptive quality controller.  Values are intentionally plain numbers so
 * callers can use this module in a worker or in a browser without shims.
 */
export const FRAME_BUDGET_MS = 16.7;
export const PERFORMANCE_PHASES = Object.freeze([
  'input',
  'layout',
  'instructions',
  'gpu',
]);
export const DEFAULT_PHASE_BUDGETS = Object.freeze({
  input: 1,
  layout: 2,
  instructions: 2,
  gpu: 5,
});
export const SAFETY_BUDGET_MS = FRAME_BUDGET_MS - Object.values(DEFAULT_PHASE_BUDGETS).reduce((sum, value) => sum + value, 0);

function finiteNonNegative(value, fallback) {
  const number = Number(value);
  return Number.isFinite(number) && number >= 0 ? number : fallback;
}

function normalizeBudgets(input) {
  const result = Object.create(null);
  for (let index = 0; index < PERFORMANCE_PHASES.length; index += 1) {
    const phase = PERFORMANCE_PHASES[index];
    result[phase] = finiteNonNegative(input?.[phase], DEFAULT_PHASE_BUDGETS[phase]);
  }
  return result;
}

function sumPhases(values) {
  let total = 0;
  for (let index = 0; index < PERFORMANCE_PHASES.length; index += 1) {
    total += finiteNonNegative(values?.[PERFORMANCE_PHASES[index]], 0);
  }
  return total;
}

/**
 * Evaluates a frame budget without retaining caller objects. `check` is safe to
 * call from a hot path; it returns a reusable result object owned by the
 * budget. `snapshot` is the explicit allocation boundary for diagnostics/HUDs.
 */
export function createPerformanceBudget({ budgets, frameBudgetMs = FRAME_BUDGET_MS } = {}) {
  const phaseBudgets = normalizeBudgets(budgets);
  const totalBudget = finiteNonNegative(frameBudgetMs, FRAME_BUDGET_MS);
  const result = {
    withinBudget: true,
    totalMs: 0,
    remainingMs: totalBudget,
    overrunMs: 0,
    phase: Object.create(null),
  };
  for (let index = 0; index < PERFORMANCE_PHASES.length; index += 1) {
    const phase = PERFORMANCE_PHASES[index];
    result.phase[phase] = { durationMs: 0, budgetMs: phaseBudgets[phase], overrunMs: 0, withinBudget: true };
  }
  let checks = 0;
  let violations = 0;

  function check(measurements = {}) {
    let total = 0;
    let overrun = 0;
    let valid = true;
    for (let index = 0; index < PERFORMANCE_PHASES.length; index += 1) {
      const phase = PERFORMANCE_PHASES[index];
      const duration = finiteNonNegative(measurements?.[phase], 0);
      const budget = phaseBudgets[phase];
      const phaseOverrun = Math.max(0, duration - budget);
      const phaseResult = result.phase[phase];
      phaseResult.durationMs = duration;
      phaseResult.budgetMs = budget;
      phaseResult.overrunMs = phaseOverrun;
      phaseResult.withinBudget = phaseOverrun === 0;
      total += duration;
      overrun += phaseOverrun;
      if (phaseOverrun > 0) valid = false;
    }
    const totalOverrun = Math.max(0, total - totalBudget);
    result.withinBudget = valid && totalOverrun === 0;
    result.totalMs = total;
    result.remainingMs = totalBudget - total;
    result.overrunMs = overrun + totalOverrun;
    checks += 1;
    if (!result.withinBudget) violations += 1;
    return result;
  }

  function snapshot() {
    const phases = Object.create(null);
    for (let index = 0; index < PERFORMANCE_PHASES.length; index += 1) {
      const phase = PERFORMANCE_PHASES[index];
      phases[phase] = { ...result.phase[phase] };
    }
    return {
      frameBudgetMs: totalBudget,
      phaseBudgets: { ...phaseBudgets },
      ...result,
      phase: phases,
      checks,
      violations,
    };
  }

  function copyMetrics(target = {}) {
    target.frameBudgetMs = totalBudget;
    target.checks = checks;
    target.violations = violations;
    target.withinBudget = result.withinBudget;
    target.totalMs = result.totalMs;
    target.remainingMs = result.remainingMs;
    target.overrunMs = result.overrunMs;
    return target;
  }

  return {
    budgets: phaseBudgets,
    frameBudgetMs: totalBudget,
    check,
    snapshot,
    copyMetrics,
    get withinBudget() { return result.withinBudget; },
  };
}

export { sumPhases };
