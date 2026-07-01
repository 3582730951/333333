export async function writeClipboard(text) {
  const value = String(text ?? '');
  if (await writeWithClipboardAPI(value)) return true;
  return writeWithTextareaFallback(value);
}

async function writeWithClipboardAPI(value) {
  try {
    if (typeof navigator === 'undefined' || typeof window === 'undefined') return false;
    if (!window.isSecureContext || !navigator.clipboard?.writeText) return false;
    await navigator.clipboard.writeText(value);
    return true;
  } catch {
    return false;
  }
}

function writeWithTextareaFallback(value) {
  let textArea = null;
  try {
    if (typeof document === 'undefined' || !document.body || typeof document.createElement !== 'function') {
      return false;
    }
    textArea = document.createElement('textarea');
    textArea.value = value;
    textArea.setAttribute('readonly', '');
    textArea.style.cssText = 'position:fixed;left:-9999px;top:0;opacity:0';
    document.body.appendChild(textArea);
    textArea.focus();
    textArea.select();
    return document.execCommand('copy');
  } catch {
    return false;
  } finally {
    try {
      if (textArea?.parentNode) textArea.parentNode.removeChild(textArea);
    } catch {
      // Ignore cleanup failures; the copy operation has already failed or completed.
    }
  }
}
