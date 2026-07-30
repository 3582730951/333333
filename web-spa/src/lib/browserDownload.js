import { setBrowserTimeout } from './browserLifecycle.js';

export function downloadBlob(name, blob) {
  if (!name || !blob || typeof document === 'undefined' || typeof URL === 'undefined') return false;

  let url = '';
  let anchor = null;
  try {
    url = URL.createObjectURL(blob);
    anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = String(name);
    anchor.rel = 'noopener';
    anchor.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0';
    document.body.appendChild(anchor);
    anchor.click();
    // Revoking synchronously after click races the browser's download
    // navigation (notably Firefox/Safari) and can leave the UI reporting
    // success without a file. Keep the object URL alive for one second; the
    // anchor itself can still be removed immediately.
    const completedURL = url;
    url = '';
    const cleanupTimer = setBrowserTimeout(() => {
      try {
        URL.revokeObjectURL(completedURL);
      } catch {
        // Object URL cleanup is best-effort after the download has started.
      }
    }, 1_000);
    if (cleanupTimer == null) {
      try {
        // A browser without timers should not retain the Blob indefinitely.
        // This rare fallback may be synchronous, but normal browsers always
        // take the delayed path above.
        URL.revokeObjectURL(completedURL);
      } catch {
        // Object URL cleanup is best-effort.
      }
    }
    return true;
  } catch {
    return false;
  } finally {
    try {
      if (anchor?.parentNode) anchor.parentNode.removeChild(anchor);
    } catch {
      // Ignore cleanup failures; the download has already failed or started.
    }
    try {
      if (url) URL.revokeObjectURL(url);
    } catch {
      // Ignore revoke failures; object URLs are best-effort browser resources.
    }
  }
}

export function downloadTextFile(name, text, type = 'text/plain;charset=utf-8') {
  try {
    return downloadBlob(name, new Blob([String(text ?? '')], { type }));
  } catch {
    return false;
  }
}
