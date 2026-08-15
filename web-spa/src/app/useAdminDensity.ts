import { useCallback, useEffect, useMemo, useState } from 'react';
import { getLocalItem, setLocalItem } from '../lib/browserStorage.js';

export type AdminDensity = 'comfortable' | 'compact';

const DENSITY_KEY = 'pool_admin_density';

function storedDensity(): AdminDensity {
  return getLocalItem(DENSITY_KEY, 'comfortable') === 'compact' ? 'compact' : 'comfortable';
}

export function useAdminDensity(forceComfortable = false) {
  const [preference, setPreferenceState] = useState<AdminDensity>(storedDensity);
  const resolved: AdminDensity = forceComfortable ? 'comfortable' : preference;

  useEffect(() => {
    setLocalItem(DENSITY_KEY, preference);
  }, [preference]);

  const setPreference = useCallback((next: AdminDensity) => setPreferenceState(next), []);
  const toggle = useCallback(() => {
    setPreferenceState((current) => current === 'compact' ? 'comfortable' : 'compact');
  }, []);

  return useMemo(() => ({ preference, resolved, setPreference, toggle }), [preference, resolved, setPreference, toggle]);
}
