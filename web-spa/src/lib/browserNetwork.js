export function browserUserAgent() {
  try {
    return typeof navigator === 'undefined' ? '' : navigator.userAgent || '';
  } catch {
    return '';
  }
}

export function browserConnection() {
  try {
    if (typeof navigator === 'undefined') return null;
    return navigator.connection || navigator.mozConnection || navigator.webkitConnection || null;
  } catch {
    return null;
  }
}

export function prefersReducedNetworkData() {
  const connection = browserConnection();
  return Boolean(connection?.saveData || /2g/.test(connection?.effectiveType || ''));
}

export function postJSONKeepalive(url, body, onError) {
  const payload = typeof body === 'string' ? body : JSON.stringify(body ?? {});
  if (sendBeaconJSON(url, payload)) return true;

  try {
    if (typeof fetch !== 'function') return false;
    fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      body: payload,
      keepalive: true,
    }).catch((error) => {
      if (typeof onError === 'function') onError(error);
    });
    return true;
  } catch (error) {
    if (typeof onError === 'function') onError(error);
    return false;
  }
}

export async function fetchText(url, options = {}) {
  if (typeof fetch !== 'function') {
    throw new Error('browser fetch API is unavailable');
  }
  const response = await fetch(url, options);
  if (!response.ok) {
    throw new Error(`request failed: HTTP ${response.status}`);
  }
  return response.text();
}

function sendBeaconJSON(url, payload) {
  try {
    if (typeof navigator === 'undefined' || typeof Blob === 'undefined') return false;
    if (typeof navigator.sendBeacon !== 'function') return false;
    return navigator.sendBeacon(url, new Blob([payload], { type: 'application/json' }));
  } catch {
    return false;
  }
}
