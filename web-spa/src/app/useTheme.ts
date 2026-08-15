import { useCallback, useEffect, useMemo, useState } from 'react';
import { getLocalItem, setLocalItem } from '../lib/browserStorage.js';
import { setDocumentElementAttribute } from '../lib/browserDocument.js';

export type ThemePreference = 'auto' | 'light' | 'dark';
export type ResolvedTheme = 'light' | 'dark';

const THEME_KEY = 'pool_theme';

function storedPreference(): ThemePreference {
  const value = getLocalItem(THEME_KEY, 'light');
  return value === 'auto' || value === 'dark' ? value : 'light';
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
  const cycle = useCallback(() => {
    setPreferenceState((current) => current === 'auto' ? 'light' : current === 'light' ? 'dark' : 'auto');
  }, []);

  return useMemo(() => ({ preference, resolved, setPreference, cycle }), [preference, resolved, setPreference, cycle]);
}
