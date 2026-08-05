import React from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { render } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import App from '../src/App';

vi.mock('../src/app/AuthProvider', () => ({
  useAuth: () => ({
    ready: false,
    authed: false,
    role: null,
    user: null,
    error: null,
    refresh: vi.fn(),
    logout: vi.fn(),
  }),
}));

vi.mock('../src/app/useTheme', () => ({
  useTheme: () => ({ resolved: 'light', preference: 'light', cycle: vi.fn() }),
}));

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({ isMobile: false, isTablet: false, isDesktop: true, collapsedByWidth: false }),
}));

vi.mock('../src/lib/i18n.js', () => ({
  getLocale: () => 'zh',
  setLocale: vi.fn(),
  t: (key: string) => key,
}));

vi.mock('../src/app/routeDefinitions', () => ({
  adminRoutes: [],
  portalRoutes: [],
  legacyRedirects: [],
  settingsSections: [],
}));

function renderAt(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}><App /></MemoryRouter>
    </QueryClientProvider>,
  );
}

describe('role-aware boot shell', () => {
  it('uses a sidebar-free shell while a portal session is loading', () => {
    const { container } = renderAt('/portal/keys');
    expect(container.querySelector('.pool-boot-shell--portal')).toBeInTheDocument();
    expect(container.querySelector('.pool-boot-sidebar')).not.toBeInTheDocument();
  });

  it('keeps the admin navigation skeleton on admin routes', () => {
    const { container } = renderAt('/accounts');
    expect(container.querySelector('.pool-boot-shell--portal')).not.toBeInTheDocument();
    expect(container.querySelector('.pool-boot-sidebar')).toBeInTheDocument();
  });
});
