import type { ReactNode } from 'react';

export type AppRole = 'admin' | 'user';
export type NavGroup = 'overview' | 'accounts' | 'access' | 'automation' | 'observability' | 'security' | 'settings' | 'portal';

export interface RouteDefinition {
  path: string;
  role: AppRole;
  navGroup: NavGroup;
  titleKey: string;
  descriptionKey: string;
  lazyLoader: () => Promise<{ default: React.ComponentType<any> }>;
  prefetch?: 'eager' | 'idle' | 'never';
  legacy?: boolean;
}

export interface ApiError extends Error {
  status: number;
  code: string;
  requestId: string;
  retryable: boolean;
  userMessage: string;
  cause?: unknown;
}

export interface PageResult<T> {
  rows: T[];
  total: number;
  page: number;
  pageSize: number;
}

export interface ResponsiveAction<T> {
  key: string;
  label: string;
  run: (row: T) => void | Promise<void>;
  mobile: 'allow' | 'desktop-only';
  destructive?: boolean;
  disabled?: (row: T) => boolean;
}

export interface ResponsiveDataView<T> {
  desktopColumns: ReadonlyArray<unknown>;
  mobileSummary: (row: T) => ReactNode;
  details: (row: T) => ReactNode;
  actions: ReadonlyArray<ResponsiveAction<T>>;
}
