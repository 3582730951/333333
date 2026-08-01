export async function writeClipboard(text) {
  const value = String(text ?? '');
  const hasClipboardAPI = typeof navigator !== 'undefined'
    && typeof window !== 'undefined'
    && window.isSecureContext
    && typeof navigator.clipboard?.writeText === 'function';

  // Calling writeText before the first await retains the click's user
  // activation. Plain HTTP deployments take the synchronous textarea path.
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
    textArea.style.cssText = 'position:fixed;left:0;top:0;width:1px;height:1px;padding:0;border:0;opacity:0';
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
