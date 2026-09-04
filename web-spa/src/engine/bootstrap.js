/*
 * This is the only Aurora file intended for the application's initial graph.
 * It has no static imports: the renderer, shaders, registry, and atmosphere
 * adapter enter only through the dynamic import below.
 */

const NOOP = () => {};

function readReducedMotion(environment) {
  try {
    return Boolean(environment.matchMedia?.('(prefers-reduced-motion: reduce)').matches);
  } catch {
    return false;
  }
}

function readSaveData(environment) {
  try {
    return Boolean(environment.navigator?.connection?.saveData);
  } catch {
    return false;
  }
}

/**
 * A non-invasive first-pass capability probe. Creating a real GL context is
 * intentionally deferred to the lazy host; an API may exist while a context is
 * refused by policy, GPU reset, or browser quota.
 */
export function probeAuroraCapabilities(environment = globalThis) {
  const navigatorObject = environment.navigator || {};
  const cores = Number(navigatorObject.hardwareConcurrency) || 4;
  const memory = Number(navigatorObject.deviceMemory) || 4;
  const reducedMotion = readReducedMotion(environment);
  const saveData = readSaveData(environment);
  const webgl2Api = typeof environment.WebGL2RenderingContext !== 'undefined';
  const severeLowEnd = cores <= 2 || memory <= 2;
  const mediumLowEnd = cores <= 4 || memory <= 4;
  let reason = 'webgl2-ready';
  if (!webgl2Api) reason = 'webgl2-api-unavailable';
  else if (saveData) reason = 'save-data';
  else if (reducedMotion) reason = 'reduced-motion';
  else if (severeLowEnd) reason = 'low-end-device';
  return {
    webgl2Api,
    cores,
    memory,
    reducedMotion,
    saveData,
    severeLowEnd,
    quality: mediumLowEnd ? 'low' : 'medium',
    eligible: webgl2Api && !saveData && !reducedMotion && !severeLowEnd,
    reason,
  };
}

function setShellState(root, state, reason) {
  if (!root || typeof root.setAttribute !== 'function') return;
  root.setAttribute('data-aurora-engine-state', state);
  root.setAttribute('data-aurora-engine-reason', reason);
}

function createFallbackSession(root, reason, onDomFallback) {
  setShellState(root, 'fallback', reason);
  const cleanup = typeof onDomFallback === 'function'
    ? onDomFallback({ state: 'fallback', reason })
    : NOOP;
  return {
    active: false,
    reason,
    dispose() {
      if (typeof cleanup === 'function') cleanup();
      setShellState(root, 'disposed', 'disposed');
    },
  };
}

/**
 * Starts progressive enhancement. Callers must render their semantic DOM and
 * ordinary CSS fallback before calling this function; failure intentionally
 * returns a small no-op session rather than blocking application functionality.
 */
export async function startAuroraEnhancement(options = {}) {
  const capabilities = probeAuroraCapabilities(options.environment || globalThis);
  const root = options.fallbackRoot || options.effectLayer || null;
  if (!capabilities.eligible) {
    return createFallbackSession(root, capabilities.reason, options.onDomFallback);
  }
  setShellState(root, 'loading', capabilities.reason);
  try {
    const { createEngineHost } = await import('./host.js');
    return await createEngineHost({ ...options, capabilities });
  } catch (error) {
    const reason = error instanceof Error ? `engine-load-failed:${error.message}` : 'engine-load-failed';
    return createFallbackSession(root, reason, options.onDomFallback);
  }
}
