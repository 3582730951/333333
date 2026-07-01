export function dispatchBrowserEvent(name, detail) {
  if (typeof window === 'undefined') return false;

  try {
    window.dispatchEvent(new CustomEvent(name, { detail }));
    return true;
  } catch {
    // Fall through to the legacy path below.
  }

  try {
    if (typeof document === 'undefined' || typeof document.createEvent !== 'function') return false;
    const event = document.createEvent('CustomEvent');
    event.initCustomEvent(name, false, false, detail);
    window.dispatchEvent(event);
    return true;
  } catch {
    return false;
  }
}
