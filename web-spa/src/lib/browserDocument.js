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
  return snapshot;
}

function verifyOverlayBodyRestored(snapshot) {
  if (!snapshot || overlayLocks.size) return;
  if (documentBodyStyle('overflow') === 'hidden') setDocumentBodyStyle('overflow', snapshot.overflow);
  if (documentBodyStyle('pointerEvents') === 'none') setDocumentBodyStyle('pointerEvents', snapshot.pointerEvents);
  setDocumentBodyAttribute('data-pool-overlay-count', null);
}

export function acquireDocumentOverlayLock(owner = 'overlay') {
  const token = Symbol(String(owner));
  if (!overlayLocks.size) {
    const overflow = documentBodyStyle('overflow');
    const pointerEvents = documentBodyStyle('pointerEvents');
    overlayBodySnapshot = {
      // Radix may install these temporary values before React effects run. They
      // are overlay state, not the page's baseline and must never be restored.
      overflow: overflow === 'hidden' ? '' : overflow,
      pointerEvents: pointerEvents === 'none' ? '' : pointerEvents,
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
  const snapshot = restoreOverlayBodySnapshot();
  if (typeof queueMicrotask === 'function') queueMicrotask(() => verifyOverlayBodyRestored(snapshot));
  if (typeof requestAnimationFrame === 'function') requestAnimationFrame(() => verifyOverlayBodyRestored(snapshot));
}

export function resetDocumentOverlayLocks() {
  overlayLocks.clear();
  const snapshot = restoreOverlayBodySnapshot();
  if (snapshot) {
    verifyOverlayBodyRestored(snapshot);
    return;
  }
  // Route changes are the final recovery boundary for a Radix layer that was
  // removed before its cleanup effect ran.
  if (documentBodyStyle('overflow') === 'hidden') setDocumentBodyStyle('overflow', '');
  if (documentBodyStyle('pointerEvents') === 'none') setDocumentBodyStyle('pointerEvents', '');
  setDocumentBodyAttribute('data-pool-overlay-count', null);
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
