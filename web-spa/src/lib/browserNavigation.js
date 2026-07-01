export function openExternalURL(url) {
  const targetURL = String(url || '').trim();
  if (!targetURL || typeof window === 'undefined') return false;

  try {
    const opened = window.open(targetURL, '_blank', 'noopener,noreferrer');
    if (opened) {
      try { opened.opener = null; } catch { /* ignore cross-browser opener assignment failures */ }
      return true;
    }
  } catch {
    return false;
  }
  return false;
}

export function browserLocation() {
  if (typeof window === 'undefined') {
    return {
      href: '',
      origin: '',
      protocol: 'http:',
      host: '',
      pathname: '/',
      hash: '',
    };
  }
  try {
    const { href, origin, protocol, host, pathname, hash } = window.location;
    return { href, origin, protocol, host, pathname, hash };
  } catch {
    return {
      href: '',
      origin: '',
      protocol: 'http:',
      host: '',
      pathname: '/',
      hash: '',
    };
  }
}

export const browserOrigin = () => browserLocation().origin;
export const browserPathname = () => browserLocation().pathname || '/';
export const browserHash = () => browserLocation().hash || '';

export function isSameOriginURL(url) {
  if (!url) return false;
  const { href, origin } = browserLocation();
  if (!href || !origin) return false;
  try {
    return new URL(url, href).origin === origin;
  } catch {
    return false;
  }
}

export function reloadPage() {
  try {
    if (typeof window === 'undefined') return false;
    window.location.reload();
    return true;
  } catch {
    return false;
  }
}

export function assignLocation(path) {
  try {
    if (typeof window === 'undefined') return false;
    window.location.assign(path);
    return true;
  } catch {
    return false;
  }
}

export function setBrowserHash(hash) {
  try {
    if (typeof window === 'undefined') return false;
    window.location.hash = String(hash || '').replace(/^#/, '');
    return true;
  } catch {
    return false;
  }
}

export function sameOriginWebSocketURL(path, query = '') {
  const { protocol, host } = browserLocation();
  if (!host) return '';
  const wsProtocol = protocol === 'https:' ? 'wss:' : 'ws:';
  return `${wsProtocol}//${host}${path}${query}`;
}
