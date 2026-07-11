import { createContext, useCallback, useContext, useEffect, useMemo, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { clearToken, isUnauthorizedError, logout as apiLogout, me } from '../api.js';
import { addWindowListener } from '../lib/browserLifecycle.js';
import { Toast } from '../components/pool/index.jsx';

export interface AuthUser {
  id?: string;
  email?: string;
  name?: string;
  role: 'admin' | 'user';
  [key: string]: unknown;
}

interface AuthContextValue {
  ready: boolean;
  authed: boolean;
  role: '' | 'admin' | 'user';
  user: AuthUser | null;
  error: unknown;
  refresh: () => Promise<unknown>;
  logout: () => Promise<void>;
}

const AUTH_QUERY_KEY = ['auth', 'me'] as const;
const AuthContext = createContext<AuthContextValue | null>(null);

async function loadCurrentUser(): Promise<AuthUser | null> {
  try {
    const response = await me({ suppressUnauthorizedEvent: true });
    if (!response || (!response.authed && response.via !== 'open')) return null;
    return { ...response, role: response.role === 'admin' ? 'admin' : 'user' } as AuthUser;
  } catch (error) {
    if (isUnauthorizedError(error)) return null;
    throw error;
  }
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const query = useQuery({ queryKey: AUTH_QUERY_KEY, queryFn: loadCurrentUser, staleTime: 30_000 });
  const refetch = query.refetch;

  useEffect(() => addWindowListener('pool-unauthorized', () => {
    clearToken();
    queryClient.setQueryData(AUTH_QUERY_KEY, null);
    queryClient.removeQueries({ predicate: (candidate) => candidate.queryKey[0] !== 'auth' });
    Toast.error('登录已失效，请重新登录。');
  }), [queryClient]);

  const refresh = useCallback(async () => {
    return refetch();
  }, [refetch]);

  const logout = useCallback(async () => {
    try { await apiLogout(); } catch { /* local session state must still be cleared */ }
    clearToken();
    queryClient.clear();
    queryClient.setQueryData(AUTH_QUERY_KEY, null);
  }, [queryClient]);

  const value = useMemo<AuthContextValue>(() => ({
    ready: !query.isPending,
    authed: Boolean(query.data),
    role: query.data?.role ?? '',
    user: query.data ?? null,
    error: query.error,
    refresh,
    logout,
  }), [query.isPending, query.data, query.error, refresh, logout]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error('useAuth must be used inside AuthProvider');
  return value;
}
