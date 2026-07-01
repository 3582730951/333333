export function createAbortController() {
  try {
    if (typeof AbortController === 'undefined') return null;
    return new AbortController();
  } catch {
    return null;
  }
}

export function abortSignal(controller) {
  return controller?.signal;
}

export function abortController(controller) {
  try {
    controller?.abort?.();
  } catch {
    // Ignore teardown failures from browser-provided abort implementations.
  }
}
