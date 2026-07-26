const noop = () => {};

const overlayLocks = new Set();
let overlayBodySnapshot = null;

function restoreOverlayBodySnapshot() {
  if (overlayLocks.size || !overlayBodySnapshot) return;
  const snapshot = overlayBodySnapshot;
  overlayBodySnapshot = null;
  setDocumentBodyStyle('overflow', snapshot.overflow);
  setDocumentBodyStyle('pointerEvents', snapshot.pointerEvents);
  setDocumentBodyAttribute('data-pool-overlay-count', null);
}

export function acquireDocumentOverlayLock(owner = 'overlay') {
  const token = Symbol(String(owner));
  if (!overlayLocks.size) {
    overlayBodySnapshot = {
      overflow: documentBodyStyle('overflow'),
      pointerEvents: documentBodyStyle('pointerEvents'),
    };
  }
  overlayLocks.add(token);
  setDocumentBodyStyle('overflow', 'hidden');
  setDocumentBodyAttribute('data-pool-overlay-count', overlayLocks.size);
  return token;
}

export function releaseDocumentOverlayLock(token) {
  if (!token || !overlayLocks.delete(token)) return;
  if (overlayLocks.size) {
    setDocumentBodyAttribute('data-pool-overlay-count', overlayLocks.size);
    return;
  }
  restoreOverlayBodySnapshot();
  if (typeof queueMicrotask === 'function') queueMicrotask(restoreOverlayBodySnapshot);
}

export function resetDocumentOverlayLocks() {
  overlayLocks.clear();
  restoreOverlayBodySnapshot();
}

export function documentOverlayLockCount() {
  return overlayLocks.size;
}

export function getDocumentElementById(id) {
  try {
    if (typeof document === 'undefined' || typeof document.getElementById !== 'function') return null;
    return document.getElementById(id);
  } catch {
    return null;
  }
}

export function requireDocumentElement(id) {
  const element = getDocumentElementById(id);
  if (!element) throw new Error(`missing document element #${id}`);
  return element;
}

export function documentBodyAttribute(name) {
  try {
    if (!name || typeof document === 'undefined' || !document.body) return '';
    return document.body.getAttribute(name) || '';
  } catch {
    return '';
  }
}

export function setDocumentBodyAttribute(name, value) {
  try {
    if (!name || typeof document === 'undefined' || !document.body) return false;
    if (value == null || value === false) {
      document.body.removeAttribute(name);
    } else {
      document.body.setAttribute(name, String(value));
    }
    return true;
  } catch {
    return false;
  }
}

export function documentBodyStyle(name) {
  try {
    if (!name || typeof document === 'undefined' || !document.body) return '';
    return document.body.style[name] || '';
  } catch {
    return '';
  }
}

export function setDocumentBodyStyle(name, value) {
  try {
    if (!name || typeof document === 'undefined' || !document.body) return false;
    document.body.style[name] = value == null ? '' : String(value);
    return true;
  } catch {
    return false;
  }
}

export function documentElementAttribute(name) {
  try {
    if (!name || typeof document === 'undefined' || !document.documentElement) return '';
    return document.documentElement.getAttribute(name) || '';
  } catch {
    return '';
  }
}

export function setDocumentElementAttribute(name, value) {
  try {
    if (!name || typeof document === 'undefined' || !document.documentElement) return false;
    if (value == null || value === false) {
      document.documentElement.removeAttribute(name);
    } else {
      document.documentElement.setAttribute(name, String(value));
    }
    return true;
  } catch {
    return false;
  }
}

export function observeDocumentElementAttributes(callback, attributeFilter = []) {
  try {
    if (typeof MutationObserver === 'undefined' || typeof callback !== 'function') return noop;
    if (typeof document === 'undefined' || !document.documentElement) return noop;
    const observer = new MutationObserver(callback);
    observer.observe(document.documentElement, {
      attributes: true,
      attributeFilter: attributeFilter.length ? attributeFilter : undefined,
    });
    return () => {
      try {
        observer.disconnect();
      } catch {
        // Observer cleanup must not break component unmount.
      }
    };
  } catch {
    return noop;
  }
}

export function observeDocumentBodyAttributes(callback, attributeFilter = []) {
  try {
    if (typeof MutationObserver === 'undefined' || typeof callback !== 'function') return noop;
    if (typeof document === 'undefined' || !document.body) return noop;
    const observer = new MutationObserver(callback);
    observer.observe(document.body, {
      attributes: true,
      attributeFilter: attributeFilter.length ? attributeFilter : undefined,
    });
    return () => {
      try {
        observer.disconnect();
      } catch {
        // Observer cleanup must not break component unmount.
      }
    };
  } catch {
    return noop;
  }
}
