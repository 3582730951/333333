const NOOP = () => {};

export const ENGINE_STATES = Object.freeze([
  'idle',
  'enhanced',
  'fallback',
  'restoring',
  'disposed',
]);

function isElement(value) {
  return Boolean(value && typeof value.setAttribute === 'function');
}

/**
 * Owns the progressive-enhancement state transition. A failure can only remove
 * the WebGL layer; it never hides, reorders, or mutates application DOM content.
 */
export function createFallbackManager({
  root = null,
  canvas = null,
  onStateChange = NOOP,
  onDomFallback = NOOP,
  onDomRestore = NOOP,
  onRestore = null,
} = {}) {
  let state = 'idle';
  let reason = 'not-started';
  let disposed = false;
  let attachedCanvas = null;
  let fallbackCleanup = NOOP;
  let restoring = false;

  const emit = () => {
    if (isElement(root)) {
      root.setAttribute('data-aurora-engine-state', state);
      root.setAttribute('data-aurora-engine-reason', reason);
    }
    onStateChange({ state, reason });
  };

  const clearFallback = () => {
    fallbackCleanup();
    fallbackCleanup = NOOP;
    onDomRestore({ state, reason });
  };

  const activateFallback = (nextReason = 'unknown') => {
    if (disposed) return;
    reason = nextReason;
    if (state !== 'fallback') {
      fallbackCleanup();
      const cleanup = onDomFallback({ state: 'fallback', reason });
      fallbackCleanup = typeof cleanup === 'function' ? cleanup : NOOP;
    }
    state = 'fallback';
    restoring = false;
    emit();
  };

  const activate = (nextReason = 'webgl2-ready') => {
    if (disposed) return;
    const wasFallback = state === 'fallback' || state === 'restoring';
    reason = nextReason;
    state = 'enhanced';
    restoring = false;
    if (wasFallback) clearFallback();
    emit();
  };

  const attemptRestore = () => {
    if (disposed || restoring || typeof onRestore !== 'function') return;
    restoring = true;
    state = 'restoring';
    reason = 'context-restoring';
    emit();
    Promise.resolve()
      .then(() => onRestore({ state, reason }))
      .then((ready) => {
        if (disposed) return;
        if (ready) activate('context-restored');
        else activateFallback('context-restore-failed');
      })
      .catch(() => activateFallback('context-restore-failed'));
  };

  const onContextLost = (event) => {
    try {
      event.preventDefault();
    } catch {
      // Some synthetic test events intentionally lack preventDefault.
    }
    activateFallback('context-lost');
  };

  const onContextRestored = () => {
    attemptRestore();
  };

  const detachCanvas = () => {
    if (!attachedCanvas || typeof attachedCanvas.removeEventListener !== 'function') return;
    attachedCanvas.removeEventListener('webglcontextlost', onContextLost);
    attachedCanvas.removeEventListener('webglcontextrestored', onContextRestored);
    attachedCanvas = null;
  };

  const attachCanvas = (nextCanvas) => {
    detachCanvas();
    if (!nextCanvas || typeof nextCanvas.addEventListener !== 'function') return;
    attachedCanvas = nextCanvas;
    attachedCanvas.addEventListener('webglcontextlost', onContextLost, false);
    attachedCanvas.addEventListener('webglcontextrestored', onContextRestored, false);
  };

  function dispose() {
    if (disposed) return;
    disposed = true;
    detachCanvas();
    fallbackCleanup();
    fallbackCleanup = NOOP;
    state = 'disposed';
    reason = 'disposed';
    emit();
  }

  attachCanvas(canvas);

  return {
    activate,
    fallback: activateFallback,
    attachCanvas,
    dispose,
    get state() { return state; },
    get reason() { return reason; },
  };
}
