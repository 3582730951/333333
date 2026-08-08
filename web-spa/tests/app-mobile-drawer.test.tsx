import React from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import App from '../src/App';
import PageHeader from '../src/components/PageHeader.jsx';

const mobileSlowRoute = vi.hoisted(() => {
  let settle: (value: any) => void = () => {};
  const promise = new Promise<any>((resolve) => { settle = resolve; });
  return { promise, resolve: (value: any) => settle(value) };
});

vi.mock('../src/app/AuthProvider', () => ({
  useAuth: () => ({
    ready: true,
    authed: true,
    role: 'admin',
    user: { email: 'operator@example.test', role: 'admin' },
    error: null,
    refresh: vi.fn(),
    logout: vi.fn(),
  }),
}));

vi.mock('../src/app/useTheme', () => ({
  useTheme: () => ({ resolved: 'light', preference: 'light', cycle: vi.fn() }),
}));

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({ isMobile: true, isTablet: false, isDesktop: false, collapsedByWidth: false }),
}));

vi.mock('../src/lib/i18n.js', () => ({
  getLocale: () => 'zh',
  setLocale: vi.fn(),
  t: (key: string) => key === 'app.page_changed' ? 'Page changed: {title}' : key,
}));

function DashboardPage() {
  return <PageHeader title="nav.dashboard" subtitle="dashboard" actions={null} />;
}

function AccountsPage() {
  return <PageHeader title="nav.account_pool" subtitle="accounts" actions={null} />;
}

function SlowPage() {
  return <PageHeader title="nav.slow" subtitle="slow" actions={null} />;
}

vi.mock('../src/app/routeDefinitions', () => ({
  adminRoutes: [
    {
      path: '/', role: 'admin', navGroup: 'overview', titleKey: 'nav.dashboard', descriptionKey: 'dashboard',
      lazyLoader: () => Promise.resolve({ default: DashboardPage }),
    },
    {
      path: '/accounts', role: 'admin', navGroup: 'accounts', titleKey: 'nav.account_pool', descriptionKey: 'accounts',
      lazyLoader: () => Promise.resolve({ default: AccountsPage }),
    },
    {
      path: '/slow', role: 'admin', navGroup: 'accounts', titleKey: 'nav.slow', descriptionKey: 'slow',
      lazyLoader: () => mobileSlowRoute.promise,
    },
  ],
  portalRoutes: [],
  legacyRedirects: [],
  settingsSections: [],
}));

describe('mobile application drawer', () => {
  it('makes the drawer modal, traps focus, and restores the trigger on Escape', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <App />
        </MemoryRouter>
      </QueryClientProvider>,
    );

    const trigger = screen.getByRole('button', { name: 'app.toggle_menu' });
    const drawer = document.getElementById('pool-mobile-navigation')!;
    const main = document.querySelector('.pool-main-layout')!;
    const skipLink = document.querySelector('a[href="#main-content"]')!;
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(drawer).toHaveAttribute('aria-hidden', 'true');
    expect(drawer).toHaveAttribute('inert');
    expect(skipLink).not.toHaveAttribute('inert');

    fireEvent.click(trigger);
    await waitFor(() => expect(drawer).toContainElement(document.activeElement as HTMLElement));
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(drawer).toHaveAttribute('aria-hidden', 'false');
    expect(drawer).toHaveAttribute('role', 'dialog');
    expect(drawer).toHaveAttribute('aria-modal', 'true');
    expect(drawer).not.toHaveAttribute('inert');
    expect(main).toHaveAttribute('inert');
    expect(skipLink).toHaveAttribute('inert');
    expect(skipLink).toHaveAttribute('aria-hidden', 'true');
    expect(skipLink).toHaveAttribute('tabindex', '-1');

    const controls = Array.from(drawer.querySelectorAll<HTMLElement>('button:not([disabled]), [tabindex]:not([tabindex="-1"])'));
    controls[0].focus();
    fireEvent.keyDown(document, { key: 'Tab', shiftKey: true });
    expect(controls.at(-1)).toHaveFocus();
    fireEvent.keyDown(document, { key: 'Tab' });
    expect(controls[0]).toHaveFocus();

    fireEvent.keyDown(document, { key: 'Escape' });
    await waitFor(() => expect(trigger).toHaveFocus());
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(drawer).toHaveAttribute('aria-hidden', 'true');
    expect(drawer).toHaveAttribute('inert');
    expect(main).not.toHaveAttribute('inert');
    expect(skipLink).not.toHaveAttribute('inert');
    expect(skipLink).not.toHaveAttribute('aria-hidden');
  });

  it('supports standard account-menu focus and keyboard navigation', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter><App /></MemoryRouter>
      </QueryClientProvider>,
    );

    const trigger = screen.getByRole('button', { name: 'app.account_menu' });
    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    const menu = await screen.findByRole('menu', { name: 'app.account_menu' });
    const items = screen.getAllByRole('menuitem');
    await waitFor(() => expect(items[0]).toHaveFocus());
    expect(trigger).toHaveAttribute('aria-controls', menu.id);

    fireEvent.keyDown(document, { key: 'ArrowDown' });
    expect(items[1]).toHaveFocus();
    fireEvent.keyDown(document, { key: 'End' });
    expect(items.at(-1)).toHaveFocus();
    fireEvent.keyDown(document, { key: 'Home' });
    expect(items[0]).toHaveFocus();
    fireEvent.keyDown(document, { key: 'Escape' });
    await waitFor(() => expect(trigger).toHaveFocus());
    expect(screen.queryByRole('menu')).not.toBeInTheDocument();
  });

  it('closes immediately for deferred drawer navigation and focuses only after commit', async () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter>
          <App />
        </MemoryRouter>
      </QueryClientProvider>,
    );

    await screen.findByRole('heading', { name: 'nav.dashboard' });
    const trigger = screen.getByRole('button', { name: 'app.toggle_menu' });
    const drawer = document.getElementById('pool-mobile-navigation')!;
    fireEvent.click(trigger);
    await waitFor(() => expect(drawer).toContainElement(document.activeElement as HTMLElement));

    fireEvent.click(screen.getByRole('button', { name: 'nav.slow' }));
    await waitFor(() => expect(trigger).toHaveAttribute('aria-expanded', 'false'));
    const liveRegion = document.querySelector('[aria-live="polite"]');
    expect(trigger).not.toHaveFocus();
    expect(screen.queryByRole('heading', { name: 'nav.slow' })).not.toBeInTheDocument();
    expect(liveRegion).toHaveTextContent('');

    await act(async () => {
      mobileSlowRoute.resolve({ default: SlowPage });
      await mobileSlowRoute.promise;
    });

    const heading = await screen.findByRole('heading', { name: 'nav.slow' });
    await waitFor(() => expect(liveRegion).toHaveTextContent('Page changed: nav.slow'));
    await waitFor(() => expect(heading).toHaveFocus());
  });
});
