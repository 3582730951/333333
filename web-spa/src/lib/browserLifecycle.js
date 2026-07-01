const noop = () => {};

function addListener(target, type, handler, options) {
  if (!type || typeof handler !== 'function') return noop;
  try {
    if (!target || typeof target.addEventListener !== 'function') return noop;
    target.addEventListener(type, handler, options);
    return () => {
      try {
        target.removeEventListener(type, handler, options);
      } catch {
        // Listener cleanup must not break React unmount paths.
      }
    };
  } catch {
    return noop;
  }
}

export function addWindowListener(type, handler, options) {
  return addListener(typeof window === 'undefined' ? null : window, type, handler, options);
}

export function addDocumentListener(type, handler, options) {
  return addListener(typeof document === 'undefined' ? null : document, type, handler, options);
}

export function documentVisibilityState() {
  try {
    return typeof document === 'undefined' ? 'visible' : document.visibilityState || 'visible';
  } catch {
    return 'visible';
  }
}

export function isDocumentVisible() {
  return documentVisibilityState() === 'visible';
}

export function browserViewportWidth(fallback = 1024) {
  try {
    return typeof window === 'undefined' ? fallback : window.innerWidth || fallback;
  } catch {
    return fallback;
  }
}

export function setBrowserTimeout(callback, delay = 0) {
  try {
    if (typeof window !== 'undefined' && typeof window.setTimeout === 'function') {
      return window.setTimeout(callback, delay);
    }
    if (typeof setTimeout === 'function') return setTimeout(callback, delay);
  } catch {
    // Ignore timer setup failures.
  }
  return null;
}

export function clearBrowserTimeout(timer) {
  if (timer == null) return;
  try {
    if (typeof window !== 'undefined' && typeof window.clearTimeout === 'function') {
      window.clearTimeout(timer);
      return;
    }
    if (typeof clearTimeout === 'function') clearTimeout(timer);
  } catch {
    // Ignore timer cleanup failures.
  }
}

export function setBrowserInterval(callback, delay = 0) {
  try {
    if (typeof window !== 'undefined' && typeof window.setInterval === 'function') {
      return window.setInterval(callback, delay);
    }
    if (typeof setInterval === 'function') return setInterval(callback, delay);
  } catch {
    // Ignore timer setup failures.
  }
  return null;
}

export function clearBrowserInterval(timer) {
  if (timer == null) return;
  try {
    if (typeof window !== 'undefined' && typeof window.clearInterval === 'function') {
      window.clearInterval(timer);
      return;
    }
    if (typeof clearInterval === 'function') clearInterval(timer);
  } catch {
    // Ignore timer cleanup failures.
  }
}

export function requestBrowserAnimationFrame(callback) {
  try {
    if (typeof window !== 'undefined' && typeof window.requestAnimationFrame === 'function') {
      return window.requestAnimationFrame(callback);
    }
  } catch {
    // Fall back to a short timer.
  }
  return setBrowserTimeout(callback, 16);
}

export function cancelBrowserAnimationFrame(frame) {
  if (frame == null) return;
  try {
    if (typeof window !== 'undefined' && typeof window.cancelAnimationFrame === 'function') {
      window.cancelAnimationFrame(frame);
      return;
    }
  } catch {
    // Fall back to timeout cleanup.
  }
  clearBrowserTimeout(frame);
}

export function requestBrowserIdleCallback(callback, options = {}) {
  try {
    if (typeof window !== 'undefined' && typeof window.requestIdleCallback === 'function') {
      return window.requestIdleCallback(callback, options);
    }
  } catch {
    // Fall back to a timeout below.
  }
  const delay = Number.isFinite(options.timeout) ? options.timeout : 1500;
  return setBrowserTimeout(() => callback({ didTimeout: false, timeRemaining: () => 0 }), delay);
}

export function cancelBrowserIdleCallback(id) {
  if (id == null) return;
  try {
    if (typeof window !== 'undefined' && typeof window.cancelIdleCallback === 'function') {
      window.cancelIdleCallback(id);
      return;
    }
  } catch {
    // Fall back to timeout cleanup.
  }
  clearBrowserTimeout(id);
}
