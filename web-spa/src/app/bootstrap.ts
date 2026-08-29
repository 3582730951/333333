import type { AuthUser } from './AuthProvider';

export interface ConsoleBootstrap {
  auth: AuthUser | null;
  role: '' | 'admin' | 'user';
  release_id: string;
  build_signature: string;
  route: string;
  theme: 'auto' | 'light' | 'dark';
  allow_registration?: boolean;
  ui_experience_v2?: boolean;
}

declare global {
  interface Window {
    __POOL_BOOTSTRAP__?: Partial<ConsoleBootstrap>;
  }
}

export function consoleBootstrap(): Partial<ConsoleBootstrap> | undefined {
  if (typeof window === 'undefined') return undefined;
  const value = window.__POOL_BOOTSTRAP__;
  if (!value || typeof value !== 'object') return undefined;
  return value;
}
