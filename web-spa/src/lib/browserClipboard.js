export async function writeClipboard(text) {
  const value = String(text ?? '');
  if (!value) return false;

  // Test the capability instead of window.isSecureContext. Managed WebViews
  // and reverse-proxied deployments can expose a working Clipboard API while
  // reporting an unexpected secure-context value.
  const hasClipboardAPI = typeof navigator !== 'undefined'
    && typeof navigator.clipboard?.writeText === 'function';

  // Calling writeText before the first await retains the click's user
  // activation. Browsers that hide or reject the API take the textarea path.
  if (hasClipboardAPI) {
    try {
      await navigator.clipboard.writeText(value);
      return true;
    } catch {
      // Browser permission policies can reject Clipboard even in a secure
      // context; keep the legacy path for managed/embedded browsers.
    }
  }
  return writeWithTextareaFallback(value);
}

function writeWithTextareaFallback(value) {
  let textArea = null;
  const previousFocus = typeof document !== 'undefined' ? document.activeElement : null;
  try {
    if (typeof document === 'undefined' || !document.body || typeof document.createElement !== 'function') {
      return false;
    }
    textArea = document.createElement('textarea');
    textArea.value = value;
    textArea.setAttribute('readonly', '');
    textArea.setAttribute('aria-hidden', 'true');
    // A 16px font avoids iOS zoom; fixed positioning and near-transparent
    // rendering keep selection available without moving the page.
    textArea.style.cssText = 'position:fixed;left:0;top:0;width:1px;height:1px;padding:0;border:0;opacity:0.01;font-size:16px;pointer-events:none';
    document.body.appendChild(textArea);
    textArea.focus();
    textArea.select();
    textArea.setSelectionRange(0, value.length);
    return typeof document.execCommand === 'function' && document.execCommand('copy') === true;
  } catch {
    return false;
  } finally {
    try {
      if (textArea?.parentNode) textArea.parentNode.removeChild(textArea);
    } catch {
      // Ignore cleanup failures; the copy operation has already failed or completed.
    }
    try {
      if (previousFocus && typeof previousFocus.focus === 'function') previousFocus.focus();
    } catch {
      // Focus restoration is best-effort.
    }
  }
}

export function selectTextForManualCopy(element) {
  try {
    if (!element || typeof element.focus !== 'function' || typeof element.select !== 'function') return false;
    element.focus();
    element.select();
    if (typeof element.setSelectionRange === 'function') {
      element.setSelectionRange(0, String(element.value ?? '').length);
    }
    return true;
  } catch {
    return false;
  }
}
