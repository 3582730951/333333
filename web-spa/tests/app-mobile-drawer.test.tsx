import React from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { describe, expect, it, vi } from 'vitest';
import App from '../src/App';

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
  t: (key: string) => key,
}));

vi.mock('../src/app/routeDefinitions', () => ({
  adminRoutes: [{
    path: '/', role: 'admin', navGroup: 'overview', titleKey: 'nav.dashboard', descriptionKey: 'dashboard',
    lazyLoader: () => Promise.resolve({ default: () => null }),
  }],
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
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
    expect(drawer).toHaveAttribute('aria-hidden', 'true');
    expect(drawer).toHaveAttribute('inert');

    fireEvent.click(trigger);
    await waitFor(() => expect(drawer).toContainElement(document.activeElement as HTMLElement));
    expect(trigger).toHaveAttribute('aria-expanded', 'true');
    expect(drawer).toHaveAttribute('aria-hidden', 'false');
    expect(drawer).toHaveAttribute('role', 'dialog');
    expect(drawer).toHaveAttribute('aria-modal', 'true');
    expect(drawer).not.toHaveAttribute('inert');
    expect(main).toHaveAttribute('inert');

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
});
