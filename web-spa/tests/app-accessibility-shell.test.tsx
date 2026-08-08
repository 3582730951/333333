import React from 'react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, useLocation, useNavigate } from 'react-router';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import App from '../src/App';
import PageHeader from '../src/components/PageHeader.jsx';
import AISettings from '../src/pages/AISettings';

const authMock = vi.hoisted(() => ({
  value: {
    ready: true,
    authed: true,
    role: 'admin' as string,
    user: { email: 'operator@example.test', role: 'admin' } as any,
    error: null as unknown,
    refresh: vi.fn(),
    logout: vi.fn(),
  },
}));

const slowRoute = vi.hoisted(() => {
  let settle: (value: any) => void = () => {};
  const promise = new Promise<any>((resolve) => { settle = resolve; });
  return { promise, resolve: (value: any) => settle(value) };
});

const settingsQueries = vi.hoisted(() => ({
  reload: vi.fn(),
  mutateAsync: vi.fn(),
}));

const networkMocks = vi.hoisted(() => ({
  postJSONKeepalive: vi.fn(() => true),
}));

vi.mock('../src/app/AuthProvider', () => ({
  useAuth: () => authMock.value,
}));

vi.mock('../src/features/settings/queries/settings', () => ({
  useAIConfigSettingsData: () => ({
    data: [],
    loading: false,
    error: null,
    reload: settingsQueries.reload,
  }),
  useSaveSettingsMutation: () => ({
    isPending: false,
    mutateAsync: settingsQueries.mutateAsync,
  }),
}));

vi.mock('../src/lib/browserNetwork.js', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../src/lib/browserNetwork.js')>();
  return { ...actual, postJSONKeepalive: networkMocks.postJSONKeepalive };
});

vi.mock('../src/app/useTheme', () => ({
  useTheme: () => ({ resolved: 'light', preference: 'light', cycle: vi.fn() }),
}));

vi.mock('../src/hooks/useResponsiveLayout.js', () => ({
  default: () => ({
    isMobile: false,
    isTablet: false,
    isDesktop: true,
    collapsedByWidth: false,
  }),
}));

vi.mock('../src/lib/i18n.js', () => ({
  getLocale: () => 'zh',
  setLocale: vi.fn(),
  t: (key: string) => key === 'app.page_changed' ? 'Page changed: {title}' : key,
}));

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="route-location">{location.pathname + location.search}</div>;
}

function ShellLocationProbe() {
  const location = useLocation();
  return <div data-testid="shell-location">{location.pathname + location.search}</div>;
}

function MockDashboardPage() {
  const navigate = useNavigate();
  return (
    <div>
      <LocationProbe />
      <PageHeader title="nav.dashboard" subtitle="dashboard subtitle" actions={null} />
      <button type="button" data-testid="to-accounts" onClick={() => navigate('/accounts')}>Go Accounts</button>
      <button type="button" data-testid="to-slow" onClick={() => navigate('/slow')}>Go Slow</button>
      <button type="button" data-testid="to-broken" onClick={() => navigate('/broken')}>Go Broken</button>
      <button type="button" data-testid="to-legacy" onClick={() => navigate('/old-accounts')}>Go Legacy</button>
    </div>
  );
}

function MockAccountsPage() {
  const navigate = useNavigate();
  return (
    <div>
      <LocationProbe />
      <PageHeader title="nav.account_pool" subtitle="accounts subtitle" actions={null} />
      <button type="button" data-testid="to-query-only" onClick={() => navigate('/accounts?page=2')}>Page 2</button>
    </div>
  );
}

function MockSettingsPage() {
  const navigate = useNavigate();
  return (
    <div>
      <LocationProbe />
      <PageHeader title="nav.settings" subtitle="settings subtitle" actions={null} />
      <button type="button" data-testid="to-settings-automation" onClick={() => navigate('/settings-v2?tab=automation')}>
        Settings Automation
      </button>
    </div>
  );
}

function SlowPage() {
  return <PageHeader title="nav.slow" subtitle="slow page" actions={null} />;
}

function BrokenPage(): never {
  throw new Error('fixture route render failure');
}

vi.mock('../src/app/routeDefinitions', () => ({
  adminRoutes: [
    {
      path: '/',
      role: 'admin',
      navGroup: 'overview',
      titleKey: 'nav.dashboard',
      descriptionKey: 'page.dashboard.desc',
      lazyLoader: () => Promise.resolve({ default: MockDashboardPage }),
    },
    {
      path: '/accounts',
      role: 'admin',
      navGroup: 'accounts',
      titleKey: 'nav.account_pool',
      descriptionKey: 'page.accounts.desc',
      lazyLoader: () => Promise.resolve({ default: MockAccountsPage }),
    },
    {
      path: '/settings-v2',
      role: 'admin',
      navGroup: 'settings',
      titleKey: 'nav.settings',
      descriptionKey: 'page.settings.desc',
      lazyLoader: () => Promise.resolve({ default: MockSettingsPage }),
    },
    {
      path: '/slow',
      role: 'admin',
      navGroup: 'overview',
      titleKey: 'nav.slow',
      descriptionKey: 'page.slow.desc',
      lazyLoader: () => slowRoute.promise,
    },
    {
      path: '/broken',
      role: 'admin',
      navGroup: 'overview',
      titleKey: 'nav.broken',
      descriptionKey: 'page.broken.desc',
      lazyLoader: () => Promise.resolve({ default: BrokenPage }),
    },
    {
      path: '/settings/ai/chatgpt',
      role: 'admin',
      navGroup: 'settings',
      titleKey: 'nav.ai_settings',
      descriptionKey: 'page.ai_settings.desc',
      lazyLoader: () => Promise.resolve({ default: AISettings }),
    },
  ],
  portalRoutes: [],
  legacyRedirects: [{ path: '/old-accounts', to: '/accounts' }],
  settingsSections: [
    { key: 'config', labelKey: 'settings.general' },
    { key: 'automation', labelKey: 'settings.automation' },
  ],
}));

function renderApp(initialPath = '/') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = () => (
    <React.StrictMode>
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={[initialPath]}>
          <App />
          <ShellLocationProbe />
        </MemoryRouter>
      </QueryClientProvider>
    </React.StrictMode>
  );
  const result = render(tree());
  return { ...result, rerenderApp: () => result.rerender(tree()) };
}

describe('accessible application shell', () => {
  beforeEach(() => {
    Object.assign(authMock.value, {
      ready: true,
      authed: true,
      role: 'admin',
      user: { email: 'operator@example.test', role: 'admin' },
      error: null,
    });
    authMock.value.refresh.mockReset();
    authMock.value.logout.mockReset();
    settingsQueries.reload.mockReset();
    settingsQueries.mutateAsync.mockReset();
    networkMocks.postJSONKeepalive.mockClear();
    document.title = '';
    document.body.focus();
  });

  it('renders one named main landmark, a skip link, and a focusable page heading without stealing initial focus', async () => {
    const { container } = renderApp('/');
    await waitFor(() => expect(screen.getByTestId('to-accounts')).toBeInTheDocument());

    expect(container.querySelectorAll('main')).toHaveLength(1);
    expect(container.querySelector('main#main-content')).toBeInTheDocument();
    const skipLink = container.querySelector<HTMLAnchorElement>('a[href="#main-content"]')!;
    expect(skipLink).toHaveClass('pool-skip-link');
    expect(container.querySelector('.pool-page-title')).toHaveAttribute('tabindex', '-1');
    expect(document.activeElement).toBe(document.body);

    fireEvent.click(skipLink);
    expect(container.querySelector('main#main-content')).toHaveFocus();
  });

  it('updates the title and live status, then focuses the new heading after pathname navigation', async () => {
    const { container } = renderApp('/');
    await waitFor(() => expect(screen.getByTestId('to-accounts')).toBeInTheDocument());

    fireEvent.click(screen.getByTestId('to-accounts'));
    await waitFor(() => expect(screen.getByTestId('route-location')).toHaveTextContent('/accounts'));
    await waitFor(() => expect(document.title).toContain('nav.account_pool'));

    expect(container.querySelector('[aria-live="polite"]')).toHaveTextContent('Page changed: nav.account_pool');
    const heading = container.querySelector('.pool-page-title');
    await waitFor(() => expect(document.activeElement).toBe(heading));
  });

  it('ignores ordinary query changes for announcements and focus', async () => {
    const { container } = renderApp('/accounts');
    await waitFor(() => expect(screen.getByTestId('to-query-only')).toBeInTheDocument());

    const button = screen.getByTestId('to-query-only');
    const liveRegion = container.querySelector('[aria-live="polite"]');
    const previousAnnouncement = liveRegion?.textContent;
    button.focus();
    fireEvent.click(button);
    await waitFor(() => expect(screen.getByTestId('route-location')).toHaveTextContent('/accounts?page=2'));

    expect(liveRegion?.textContent).toBe(previousAnnouncement);
    expect(document.activeElement).toBe(button);
  });

  it('treats the settings tab as route identity for title, announcement, and focus', async () => {
    const { container } = renderApp('/settings-v2?tab=config');
    await waitFor(() => expect(screen.getByTestId('to-settings-automation')).toBeInTheDocument());

    fireEvent.click(screen.getByTestId('to-settings-automation'));
    await waitFor(() => expect(screen.getByTestId('route-location')).toHaveTextContent('/settings-v2?tab=automation'));
    await waitFor(() => expect(document.title).toContain('settings.automation'));

    expect(container.querySelector('[aria-live="polite"]')).toHaveTextContent('settings.automation');
    const heading = container.querySelector('.pool-page-title');
    await waitFor(() => expect(document.activeElement).toBe(heading));
  });

  it('waits for a deferred lazy page commit and focuses it once under StrictMode', async () => {
    const focusSpy = vi.spyOn(HTMLElement.prototype, 'focus');
    const { container } = renderApp('/');
    await screen.findByTestId('to-slow');
    focusSpy.mockClear();

    fireEvent.click(screen.getByTestId('to-slow'));
    await act(async () => { await Promise.resolve(); });

    const main = container.querySelector('main#main-content');
    const liveRegion = container.querySelector('[aria-live="polite"]');
    expect(screen.queryByRole('heading', { name: 'nav.slow' })).not.toBeInTheDocument();
    expect(liveRegion).toHaveTextContent('');
    expect(focusSpy.mock.instances).not.toContain(main);

    await act(async () => {
      slowRoute.resolve({ default: SlowPage });
      await slowRoute.promise;
    });

    const heading = await screen.findByRole('heading', { name: 'nav.slow' });
    expect(screen.getByTestId('shell-location')).toHaveTextContent('/slow');
    await waitFor(() => expect(heading).toHaveFocus());
    expect(liveRegion).toHaveTextContent('Page changed: nav.slow');
    expect(focusSpy.mock.instances.filter((instance) => instance === heading)).toHaveLength(1);
  });

  it('announces and focuses the final target of a legacy redirect only once', async () => {
    const focusSpy = vi.spyOn(HTMLElement.prototype, 'focus');
    const { container } = renderApp('/');
    await screen.findByTestId('to-legacy');
    focusSpy.mockClear();

    fireEvent.click(screen.getByTestId('to-legacy'));
    const heading = await screen.findByRole('heading', { name: 'nav.account_pool' });
    await waitFor(() => expect(screen.getByTestId('shell-location')).toHaveTextContent('/accounts'));
    await waitFor(() => expect(heading).toHaveFocus());

    expect(container.querySelector('[aria-live="polite"]')).toHaveTextContent('Page changed: nav.account_pool');
    expect(focusSpy.mock.instances.filter((instance) => instance === heading)).toHaveLength(1);
  });

  it('commits the page error fallback with one focusable h1', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const { container } = renderApp('/');
    await screen.findByTestId('to-broken');

    fireEvent.click(screen.getByTestId('to-broken'));
    const heading = await screen.findByRole('heading', { name: '页面遇到错误' });
    await waitFor(() => expect(heading).toHaveFocus());

    expect(container.querySelectorAll('main')).toHaveLength(1);
    expect(container.querySelectorAll('main h1')).toHaveLength(1);
    expect(container.querySelector('[aria-live="polite"]')).toHaveTextContent('Page changed: nav.broken');
  });

  it('moves from Login to Dashboard on the same pathname after the page commits', async () => {
    Object.assign(authMock.value, {
      ready: true,
      authed: false,
      role: '',
      user: null,
      error: null,
    });
    const { container, rerenderApp } = renderApp('/');
    const loginHeading = await screen.findByRole('heading', { name: 'auth.login_title' });

    expect(container.querySelectorAll('main')).toHaveLength(1);
    expect(loginHeading).toHaveAttribute('tabindex', '-1');

    Object.assign(authMock.value, {
      authed: true,
      role: 'admin',
      user: { email: 'operator@example.test', role: 'admin' },
    });
    rerenderApp();

    const dashboardHeading = await screen.findByRole('heading', { name: 'nav.dashboard' });
    await waitFor(() => expect(dashboardHeading).toHaveFocus());
    expect(container.querySelector('[aria-live="polite"]')).toHaveTextContent('Page changed: nav.dashboard');
  });

  it('keeps initial boot session restoration silent', async () => {
    Object.assign(authMock.value, {
      ready: false,
      authed: false,
      role: '',
      user: null,
      error: null,
    });
    const { container, rerenderApp } = renderApp('/');
    expect(container.querySelector('.pool-boot-main')).toBeInTheDocument();

    Object.assign(authMock.value, {
      ready: true,
      authed: true,
      role: 'admin',
      user: { email: 'operator@example.test', role: 'admin' },
    });
    rerenderApp();

    await screen.findByRole('heading', { name: 'nav.dashboard' });
    expect(container.querySelector('[aria-live="polite"]')).toHaveTextContent('');
    expect(document.activeElement).toBe(document.body);
  });

  it('gives the authentication error view one main and one focusable h1', async () => {
    Object.assign(authMock.value, {
      ready: true,
      authed: false,
      role: '',
      user: null,
      error: new Error('fixture auth failure'),
    });
    const { container } = renderApp('/');
    const heading = await screen.findByRole('heading', { name: 'error.console_connection' });

    expect(container.querySelectorAll('main')).toHaveLength(1);
    expect(container.querySelectorAll('main h1')).toHaveLength(1);
    expect(heading).toHaveAttribute('tabindex', '-1');
  });

  it('keeps the real AI settings page inside the shell main without nesting landmarks', async () => {
    const { container } = renderApp('/settings/ai/chatgpt');
    const heading = await screen.findByRole('heading', { name: 'ai_settings.title' });

    expect(container.querySelectorAll('main')).toHaveLength(1);
    expect(container.querySelectorAll('main h1')).toHaveLength(1);
    expect(heading).toHaveAttribute('tabindex', '-1');
  });
});
