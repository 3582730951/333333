import { useCallback, useEffect, useMemo, useState } from 'react';
import { getLocalItem, setLocalItem } from '../lib/browserStorage.js';
import { setDocumentElementAttribute } from '../lib/browserDocument.js';

export type ThemePreference = 'auto' | 'light' | 'dark';
export type ResolvedTheme = 'light' | 'dark';

const THEME_KEY = 'pool_theme';

// Dark is the product's default surface, not a preference the user has to find:
// the console is a dark-first curatorial shell and every token block is authored
// against it. `light` stays a first-class explicit choice, `auto` still follows
// the OS, and an unrecognised stored value falls back to dark rather than light.
function storedPreference(): ThemePreference {
  const value = getLocalItem(THEME_KEY, 'dark');
  return value === 'auto' || value === 'light' ? value : 'dark';
}

function systemTheme(): ResolvedTheme {
  return typeof window !== 'undefined' && window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

export function useTheme() {
  const [preference, setPreferenceState] = useState<ThemePreference>(storedPreference);
  const [system, setSystem] = useState<ResolvedTheme>(systemTheme);
  const resolved = preference === 'auto' ? system : preference;

  useEffect(() => {
    const media = window.matchMedia?.('(prefers-color-scheme: dark)');
    if (!media) return undefined;
    const sync = () => setSystem(media.matches ? 'dark' : 'light');
    sync();
    media.addEventListener?.('change', sync);
    return () => media.removeEventListener?.('change', sync);
  }, []);

  useEffect(() => {
    setLocalItem(THEME_KEY, preference);
    setDocumentElementAttribute('data-theme', resolved);
    setDocumentElementAttribute('data-theme-preference', preference);
  }, [preference, resolved]);

  const setPreference = useCallback((next: ThemePreference) => setPreferenceState(next), []);
  // Cycle order follows the new default: dark -> light -> auto -> dark.
  const cycle = useCallback(() => {
    setPreferenceState((current) => current === 'dark' ? 'light' : current === 'light' ? 'auto' : 'dark');
  }, []);

  return useMemo(() => ({ preference, resolved, setPreference, cycle }), [preference, resolved, setPreference, cycle]);
}
