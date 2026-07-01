const memoryStores = {
  local: new Map(),
  session: new Map(),
};

function storageFor(kind) {
  if (typeof window === 'undefined') return null;
  try {
    return kind === 'session' ? window.sessionStorage : window.localStorage;
  } catch {
    return null;
  }
}

function memoryFor(kind) {
  return kind === 'session' ? memoryStores.session : memoryStores.local;
}

function getItem(kind, key, fallback = '') {
  try {
    const value = storageFor(kind)?.getItem(key);
    if (value != null) return value;
  } catch {
    // Fall back to the per-page in-memory store below.
  }
  return memoryFor(kind).get(key) ?? fallback;
}

function setItem(kind, key, value) {
  const normalized = String(value ?? '');
  memoryFor(kind).set(key, normalized);
  try {
    storageFor(kind)?.setItem(key, normalized);
  } catch {
    // Keeping the memory copy is enough for the current page lifetime.
  }
}

function removeItem(kind, key) {
  memoryFor(kind).delete(key);
  try {
    storageFor(kind)?.removeItem(key);
  } catch {
    // Nothing else to clean up when browser storage is unavailable.
  }
}

export const getLocalItem = (key, fallback = '') => getItem('local', key, fallback);
export const setLocalItem = (key, value) => setItem('local', key, value);
export const removeLocalItem = (key) => removeItem('local', key);

export const getSessionItem = (key, fallback = '') => getItem('session', key, fallback);
export const setSessionItem = (key, value) => setItem('session', key, value);
export const removeSessionItem = (key) => removeItem('session', key);
