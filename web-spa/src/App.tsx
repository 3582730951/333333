import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState, type ComponentType, type CSSProperties, type LazyExoticComponent, type ReactNode } from 'react';
import { Navigate, Route, Routes, useLocation, useNavigate } from 'react-router';
import { useIsFetching } from '@tanstack/react-query';
import * as PoolUI from './components/pool/index.jsx';
import {
  IconChevronDown, IconClose, IconExit, IconGlobe, IconHistogram, IconHome, IconKey, IconLanguage,
  IconList, IconMoon, IconPulse, IconSetting, IconSun, IconUser, IconUserGroup,
} from './components/pool/icons.jsx';
import AppErrorBoundary, { isChunkLoadError, notifyChunkUpdateAvailable, reportClientError } from './components/AppErrorBoundary.jsx';
import LoadErrorBanner from './components/LoadErrorBanner.jsx';
import { useAuth } from './app/AuthProvider';
import { adminRoutes, legacyRedirects, portalRoutes, settingsSections } from './app/routeDefinitions';
import { useTheme } from './app/useTheme';
import useResponsiveLayout from './hooks/useResponsiveLayout.js';
import { getLocale, setLocale, t } from './lib/i18n.js';
import { addDocumentListener, addWindowListener, cancelBrowserIdleCallback, requestBrowserIdleCallback } from './lib/browserLifecycle.js';
import { prefersReducedNetworkData } from './lib/browserNetwork.js';
import { resetDocumentOverlayLocks } from './lib/browserDocument.js';
import type { RouteDefinition } from './model/contracts';

const { Avatar, Button, Layout, Nav, Toast } = PoolUI as any;
const { Header, Sider, Content } = Layout;
const SIDEBAR_EXPANDED_WIDTH = 248;
const SIDEBAR_COLLAPSED_WIDTH = 68;

const adminPages = new Map<string, LazyExoticComponent<ComponentType<any>>>(adminRoutes.map((route) => [route.path, lazy(route.lazyLoader)]));
const portalPages = new Map<string, LazyExoticComponent<ComponentType<any>>>(portalRoutes.map((route) => [route.path, lazy(route.lazyLoader)]));
const LoginPage = lazy(() => import('./pages/Login.jsx'));

const ADMIN_GROUPS = [
  { key: 'accounts', labelKey: 'nav.accounts', icon: IconUserGroup },
  { key: 'access', labelKey: 'nav.access', icon: IconKey },
  { key: 'automation', labelKey: 'nav.automation_group', icon: IconPulse },
  { key: 'observability', labelKey: 'nav.observability', icon: IconHistogram },
  { key: 'security', labelKey: 'nav.access_control', icon: IconUserGroup },
] as const;

type NavigationItem = {
  itemKey: string;
  text: string;
  icon?: ReactNode;
  items?: NavigationItem[];
};

type CommittedViewPayload = {
  identity: string;
  title: string;
};

function CommittedView({
  identity,
  title,
  onCommit,
  children,
}: {
  identity: string;
  title: string;
  onCommit: (payload: CommittedViewPayload) => void;
  children: ReactNode;
}) {
  const onCommitRef = useRef(onCommit);
  const titleRef = useRef(title);
  onCommitRef.current = onCommit;
  titleRef.current = title;

  useEffect(() => {
    onCommitRef.current({ identity, title: titleRef.current });
  }, [identity]);

  return <>{children}</>;
}

function RouteStatus({ announcement }: { announcement: string }) {
  return <div className="pool-sr-only" role="status" aria-live="polite" aria-atomic="true">{announcement}</div>;
}

function reportPrefetchError(error: unknown, route: RouteDefinition) {
  reportClientError(error, { source: 'route.prefetch', componentStack: `prefetch:${route.path}` });
  if (isChunkLoadError(error)) notifyChunkUpdateAvailable(error);
}

function RouteFallback() {
  return (
    <div className="pool-route-fallback" data-page-ready="false" aria-label={t('common.loading')}>
      <div className="pool-skel pool-route-fallback__title" />
      <div className="pool-route-fallback__metrics">
        {Array.from({ length: 4 }).map((_, index) => <div className="pool-skel pool-route-fallback__metric" key={index} />)}
      </div>
      <div className="pool-skel pool-route-fallback__content" />
    </div>
  );
}

function StablePageReady({ routeKey, children }: { routeKey: string; children: ReactNode }) {
  const fetching = useIsFetching({ predicate: (query) => query.queryKey[0] !== 'auth' });
  const [ready, setReady] = useState(false);
  const [contentBusy, setContentBusy] = useState(true);
  const contentRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    setContentBusy(true);
    const node = contentRef.current;
    if (!node) return undefined;
    const sync = () => {
      const text = node.textContent || '';
      const pendingChart = text.includes('加载图表') || text.includes('Loading chart');
      const pendingSkeleton = Boolean(node.querySelector('.pool-skeleton-table, .pool-route-fallback'));
      setContentBusy(pendingChart || pendingSkeleton);
    };
    const observer = new MutationObserver(sync);
    observer.observe(node, { childList: true, subtree: true, characterData: true });
    queueMicrotask(sync);
    return () => observer.disconnect();
  }, [routeKey]);

  useEffect(() => {
    setReady(false);
    if (fetching > 0 || contentBusy) return undefined;
    let frame = 0;
    const timer = window.setTimeout(() => {
      frame = window.requestAnimationFrame(() => setReady(true));
    }, 180);
    return () => {
      window.clearTimeout(timer);
      if (frame) window.cancelAnimationFrame(frame);
    };
  }, [routeKey, fetching, contentBusy]);

  return <div ref={contentRef} className="pool-route-content" data-page-ready={ready ? 'true' : 'false'}>{children}</div>;
}

function BootScreen({ portal = false }: { portal?: boolean }) {
  return (
    <div className={`pool-boot-shell ${portal ? 'pool-boot-shell--portal' : ''}`} data-page-ready="false">
      {!portal ? <aside className="pool-boot-sidebar">
        <div className="pool-boot-brand"><span className="pool-skel" /><span className="pool-skel" /></div>
        {Array.from({ length: 7 }).map((_, index) => <div className="pool-skel pool-boot-nav" key={index} />)}
      </aside> : null}
      <main className="pool-boot-main">
        <header className="pool-boot-header"><span className="pool-skel" /><span className="pool-skel" /></header>
        <RouteFallback />
      </main>
    </div>
  );
}

function adminNavigation(): NavigationItem[] {
  const overview = adminRoutes.find((route) => route.path === '/');
  const items: NavigationItem[] = overview ? [{ itemKey: '/', text: t(overview.titleKey), icon: <IconHome /> }] : [];
  items.push({ itemKey: '/public-chat', text: t('nav.public_chat'), icon: <IconGlobe /> });
  for (const group of ADMIN_GROUPS) {
    const Icon = group.icon;
    const children = adminRoutes
      .filter((route) => route.navGroup === group.key && !('navHidden' in route && route.navHidden))
      .map((route) => ({ itemKey: route.path, text: t(route.titleKey) }));
    if (children.length) items.push({ itemKey: `group:${group.key}`, text: t(group.labelKey), icon: <Icon />, items: children });
  }
  items.push({ itemKey: '/settings/ai/chatgpt', text: t('nav.ai_settings'), icon: <IconSetting /> });
  items.push({
    itemKey: 'group:settings',
    text: t('nav.settings'),
    icon: <IconSetting />,
    items: settingsSections.map((section) => ({
      itemKey: `/settings-v2?tab=${section.key}`,
      text: t(section.labelKey),
    })),
  });
  return items;
}

function portalNavigation(): NavigationItem[] {
  const icons: Record<string, ComponentType<any>> = {
    '/portal': IconHistogram,
    '/portal/keys': IconKey,
    '/portal/models': IconSetting,
    '/portal/profile': IconUser,
  };
  return portalRoutes.map((route) => {
    const Icon = icons[route.path] || IconList;
    return { itemKey: route.path, text: t(route.titleKey), icon: <Icon /> };
  });
}

function AppRoutes({
  admin,
  routeIdentity,
  routeTitle,
  onViewCommit,
}: {
  admin: boolean;
  routeIdentity: string;
  routeTitle: string;
  onViewCommit: (payload: CommittedViewPayload) => void;
}) {
  const location = useLocation();
  const routes: ReadonlyArray<RouteDefinition> = admin ? adminRoutes : portalRoutes;
  const pages = admin ? adminPages : portalPages;
  const fallback = admin ? '/' : '/portal';
  const shellIdentity = `shell:${admin ? 'admin' : 'portal'}:${routeIdentity}`;
  return (
    <AppErrorBoundary
      variant="page"
      resetKey={`${location.pathname}${location.search}`}
      onFallbackCommit={() => onViewCommit({ identity: shellIdentity, title: routeTitle })}
    >
      <Suspense fallback={<RouteFallback />}>
        <StablePageReady routeKey={`${location.pathname}${location.search}`}>
          <Routes>
            {routes.map((route) => {
              const Page = pages.get(route.path)!;
              return (
                <Route
                  key={route.path}
                  path={route.path}
                  element={(
                    <CommittedView identity={shellIdentity} title={routeTitle} onCommit={onViewCommit}>
                      <Page />
                    </CommittedView>
                  )}
                />
              );
            })}
            {admin ? legacyRedirects.map((redirect) => (
              <Route key={redirect.path} path={redirect.path} element={<Navigate to={redirect.to} replace />} />
            )) : null}
            <Route path="*" element={<Navigate to={fallback} replace />} />
          </Routes>
        </StablePageReady>
      </Suspense>
    </AppErrorBoundary>
  );
}

export default function App() {
  const auth = useAuth();
  const theme = useTheme();
  const responsive = useResponsiveLayout();
  const navigate = useNavigate();
  const location = useLocation();
  const [locale, setLocaleState] = useState(() => getLocale());
  const [collapsed, setCollapsed] = useState(responsive.collapsedByWidth);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [accountMenuOpen, setAccountMenuOpen] = useState(false);
  const [aiSettingsDirty, setAISettingsDirty] = useState(false);
  const [routeAnnouncement, setRouteAnnouncement] = useState('');
  const accountMenuRef = useRef<HTMLDivElement | null>(null);
  const accountMenuButtonRef = useRef<HTMLButtonElement | null>(null);
  const mobileMenuButtonRef = useRef<HTMLButtonElement | null>(null);
  const wasMobileRef = useRef(responsive.isMobile);
  const isAdmin = auth.role === 'admin';

  const closeMobileMenu = useCallback(() => {
    setMobileOpen(false);
    queueMicrotask(() => mobileMenuButtonRef.current?.focus());
  }, []);

  const closeAccountMenuAndRestoreFocus = useCallback(() => {
    setAccountMenuOpen(false);
    queueMicrotask(() => accountMenuButtonRef.current?.focus());
  }, []);

  useEffect(() => {
    resetDocumentOverlayLocks();
    return resetDocumentOverlayLocks;
  }, [location.pathname, location.search]);

  useEffect(() => addWindowListener('pool-locale-change', (event: CustomEvent<string>) => setLocaleState(event.detail === 'en' ? 'en' : 'zh')), []);
  useEffect(() => addWindowListener('pool-ai-settings-dirty', (event: CustomEvent<boolean>) => setAISettingsDirty(Boolean(event.detail))), []);
  useEffect(() => {
    setCollapsed(responsive.collapsedByWidth);
    if (!responsive.isMobile) {
      const crossedToDesktopWithDrawerOpen = wasMobileRef.current && mobileOpen;
      setMobileOpen(false);
      if (crossedToDesktopWithDrawerOpen) {
        queueMicrotask(() => {
          const currentNavigationItem = document.querySelector<HTMLElement>('#pool-desktop-navigation [aria-current="page"]');
          (currentNavigationItem || accountMenuButtonRef.current)?.focus();
        });
      }
    }
    wasMobileRef.current = responsive.isMobile;
  }, [mobileOpen, responsive.collapsedByWidth, responsive.isMobile]);

  useEffect(() => {
    if (!responsive.isMobile || !mobileOpen) return undefined;
    const drawer = document.getElementById('pool-mobile-navigation');
    const focusFirstDrawerControl = () => {
      drawer?.querySelector<HTMLElement>('button:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])')?.focus();
    };
    queueMicrotask(focusFirstDrawerControl);
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        closeMobileMenu();
        return;
      }
      if (event.key !== 'Tab' || !drawer) return;
      const controls = Array.from(drawer.querySelectorAll<HTMLElement>('button:not([disabled]), a[href], [tabindex]:not([tabindex="-1"])'));
      if (!controls.length) return;
      const first = controls[0];
      const last = controls[controls.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    return addDocumentListener('keydown', closeOnEscape);
  }, [closeMobileMenu, mobileOpen, responsive.isMobile]);

  useEffect(() => {
    if (!accountMenuOpen) return undefined;
    const menu = accountMenuRef.current?.querySelector<HTMLElement>('[role="menu"]');
    queueMicrotask(() => menu?.querySelector<HTMLElement>('[role="menuitem"]')?.focus());
    const closeOnOutside = (event: PointerEvent) => {
      if (!accountMenuRef.current?.contains(event.target as Node)) setAccountMenuOpen(false);
    };
    const handleKeyboard = (event: KeyboardEvent) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        setAccountMenuOpen(false);
        queueMicrotask(() => accountMenuButtonRef.current?.focus());
        return;
      }
      if (event.key === 'Tab') {
        setAccountMenuOpen(false);
        return;
      }
      if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
      const items = Array.from(menu?.querySelectorAll<HTMLElement>('[role="menuitem"]') || []);
      if (!items.length) return;
      event.preventDefault();
      const current = items.indexOf(document.activeElement as HTMLElement);
      const next = event.key === 'Home' ? 0
        : event.key === 'End' ? items.length - 1
          : event.key === 'ArrowDown' ? (current + 1 + items.length) % items.length
            : (current - 1 + items.length) % items.length;
      items[next].focus();
    };
    const removePointer = addDocumentListener('pointerdown', closeOnOutside);
    const removeKeyboard = addDocumentListener('keydown', handleKeyboard);
    return () => { removePointer(); removeKeyboard(); };
  }, [accountMenuOpen]);

  useEffect(() => {
    if (!auth.authed || prefersReducedNetworkData()) return undefined;
    const routes = (isAdmin ? adminRoutes : portalRoutes) as ReadonlyArray<RouteDefinition>;
    const preloadRoutes = routes.filter((route) => route.prefetch === 'eager' || route.prefetch === 'idle');
    const idle = requestBrowserIdleCallback(() => preloadRoutes.forEach((route) => route.lazyLoader().catch((error) => reportPrefetchError(error, route))), { timeout: 3000 });
    return () => cancelBrowserIdleCallback(idle);
  }, [auth.authed, isAdmin]);

  const switchLocale = useCallback(() => {
    const next = locale === 'en' ? 'zh' : 'en';
    setLocale(next);
    setLocaleState(next);
  }, [locale]);

  const navigation = useMemo(() => isAdmin ? adminNavigation() : portalNavigation(), [isAdmin, locale]);
  const activeSettingsTab = new URLSearchParams(location.search).get('tab') || 'config';
  const currentNavKey = location.pathname === '/settings-v2'
    ? `/settings-v2?tab=${activeSettingsTab}`
    : location.pathname.startsWith('/settings/ai/')
      ? '/settings/ai/chatgpt'
      : location.pathname;
  const activeRoute = (isAdmin ? adminRoutes : portalRoutes).find((route) => route.path === location.pathname);
  const activeSettingsSection = location.pathname === '/settings-v2'
    ? settingsSections.find((section) => section.key === activeSettingsTab)
    : undefined;
  const routeIdentity = location.pathname === '/settings-v2'
    ? `/settings-v2?tab=${activeSettingsTab}`
    : location.pathname;
  const routeTitle = activeSettingsSection
    ? `${t('nav.settings')} · ${t(activeSettingsSection.labelKey)}`
    : activeRoute
      ? t(activeRoute.titleKey)
      : t('app.title');
  const shellViewIdentity = `shell:${isAdmin ? 'admin' : 'portal'}:${routeIdentity}`;
  const currentViewIdentity = !auth.ready
    ? 'auth:boot'
    : auth.error
      ? 'auth:error'
      : auth.authed
        ? shellViewIdentity
        : 'auth:login';
  const currentViewTitle = !auth.ready
    ? t('app.title')
    : auth.error
      ? t('error.console_connection')
      : auth.authed
        ? routeTitle
        : t('auth.login_title');
  const lastCommittedViewRef = useRef<string | null>(null);
  const pendingCommittedViewRef = useRef<CommittedViewPayload | null>(null);
  const expectedViewIdentityRef = useRef(currentViewIdentity);
  const previousRequestedShellViewRef = useRef(shellViewIdentity);
  expectedViewIdentityRef.current = currentViewIdentity;

  useEffect(() => {
    const appTitle = t('app.title');
    document.title = currentViewTitle === appTitle ? appTitle : `${currentViewTitle} – ${appTitle}`;
  }, [currentViewTitle, locale]);

  const applyCommittedView = useCallback((payload: CommittedViewPayload) => {
    if (lastCommittedViewRef.current === payload.identity) return;
    const isInitialCommit = lastCommittedViewRef.current === null;
    lastCommittedViewRef.current = payload.identity;
    if (isInitialCommit) return;

    setRouteAnnouncement(t('app.page_changed').replace('{title}', payload.title));
    const main = document.getElementById('main-content');
    const heading = main?.querySelector<HTMLElement>('h1');
    (heading || main)?.focus();
  }, []);

  const handleViewCommit = useCallback((payload: CommittedViewPayload) => {
    if (lastCommittedViewRef.current === payload.identity) return;
    if (lastCommittedViewRef.current === null) {
      applyCommittedView(payload);
      return;
    }
    if (payload.identity.startsWith('shell:admin:') && responsive.isMobile && mobileOpen) {
      pendingCommittedViewRef.current = payload;
      setMobileOpen(false);
      return;
    }
    applyCommittedView(payload);
  }, [applyCommittedView, mobileOpen, responsive.isMobile]);

  useEffect(() => {
    if (mobileOpen) return;
    const pending = pendingCommittedViewRef.current;
    if (!pending) return;
    pendingCommittedViewRef.current = null;
    if (pending.identity !== expectedViewIdentityRef.current) return;
    applyCommittedView(pending);
  }, [applyCommittedView, mobileOpen]);

  useEffect(() => {
    const previous = previousRequestedShellViewRef.current;
    previousRequestedShellViewRef.current = shellViewIdentity;
    if (previous === shellViewIdentity) return;
    if (auth.authed && isAdmin && responsive.isMobile && mobileOpen) {
      setMobileOpen(false);
    }
  }, [auth.authed, isAdmin, mobileOpen, responsive.isMobile, shellViewIdentity]);
  const ident = auth.user?.email || auth.user?.name || (isAdmin ? 'admin' : 'user');
  const identInitial = String(ident).trim().charAt(0).toUpperCase();
  const navCollapsed = isAdmin && !responsive.isMobile ? collapsed : false;
  const sidebarWidth = !isAdmin ? 0 : responsive.isMobile ? SIDEBAR_EXPANDED_WIDTH : navCollapsed ? SIDEBAR_COLLAPSED_WIDTH : SIDEBAR_EXPANDED_WIDTH;
  const navigateFromShell = (target: string) => {
    if (target === `${location.pathname}${location.search}` || target === location.pathname) return;
    if (aiSettingsDirty && !window.confirm(t('ai_settings.leave_description'))) return;
    setAISettingsDirty(false);
    navigate(target);
    if (responsive.isMobile && isAdmin) setMobileOpen(false);
  };

  if (!auth.ready) return <BootScreen portal={location.pathname.startsWith('/portal')} />;
  if (auth.error) {
    return (
      <>
        <RouteStatus announcement={routeAnnouncement} />
        <CommittedView identity="auth:error" title={t('error.console_connection')} onCommit={handleViewCommit}>
          <main id="main-content" tabIndex={-1} className="pool-auth-error">
            <div className="pool-auth-error__brand"><Avatar size="small">P</Avatar><strong>{t('app.title')}</strong></div>
            <h1 className="pool-page-title" tabIndex={-1}>{t('error.console_connection')}</h1>
            <LoadErrorBanner error={auth.error} onRetry={auth.refresh} title={t('error.console_connection')} />
          </main>
        </CommittedView>
      </>
    );
  }
  if (!auth.authed) {
    return (
      <>
        <RouteStatus announcement={routeAnnouncement} />
        <Suspense fallback={<BootScreen portal={location.pathname.startsWith('/portal')} />}>
          <CommittedView identity="auth:login" title={t('auth.login_title')} onCommit={handleViewCommit}>
            <LoginPage onSuccess={auth.refresh} />
          </CommittedView>
        </Suspense>
      </>
    );
  }

  const layoutStyle = { '--pool-sidebar-width': `${sidebarWidth}px` } as CSSProperties;
  const accountMenu = (
    <div className="pool-account-menu-wrap" ref={accountMenuRef}>
      <Button ref={accountMenuButtonRef} className="pool-account-menu-button" theme="borderless" aria-label={t('app.account_menu')} aria-haspopup="menu" aria-controls="pool-account-menu" aria-expanded={accountMenuOpen} onClick={() => setAccountMenuOpen((value) => !value)} onKeyDown={(event: React.KeyboardEvent) => {
        if (event.key !== 'ArrowDown') return;
        event.preventDefault();
        setAccountMenuOpen(true);
      }}>
        <Avatar size="extra-small" className="pool-account-avatar">{identInitial}</Avatar><span className="pool-account-menu-ident">{ident}</span><IconChevronDown className="pool-account-menu-caret" />
      </Button>
      {accountMenuOpen ? (
        <div id="pool-account-menu" className="pool-account-menu" role="menu" aria-label={t('app.account_menu')}>
          <div className="pool-account-menu-title"><span>{ident}</span><small>{isAdmin ? t('role.admin') : t('role.user')}</small></div>
          <div className="pool-account-menu-divider" />
          <button type="button" className="pool-account-menu-item" role="menuitem" tabIndex={-1} onClick={() => { switchLocale(); closeAccountMenuAndRestoreFocus(); }}><IconLanguage /><span>{locale === 'en' ? '切换到中文' : 'Switch to English'}</span></button>
          <button type="button" className="pool-account-menu-item" role="menuitem" tabIndex={-1} onClick={() => { theme.cycle(); closeAccountMenuAndRestoreFocus(); }}>{theme.resolved === 'dark' ? <IconMoon /> : <IconSun />}<span>{t(`theme.${theme.preference}`)}</span></button>
          <div className="pool-account-menu-divider" />
          <button type="button" className="pool-account-menu-item" role="menuitem" tabIndex={-1} onClick={async () => { setAccountMenuOpen(false); await auth.logout(); navigate('/'); Toast.success(t('app.logged_out')); }}><IconExit /><span>{t('app.logout')}</span></button>
        </div>
      ) : null}
    </div>
  );
  return (
    <Layout className={`pool-app-layout ${responsive.isMobile ? 'pool-app-mobile' : 'pool-app-desktop'} ${isAdmin ? 'pool-admin-shell' : 'pool-portal-shell'} ${navCollapsed ? 'pool-sidebar-is-collapsed' : ''}`} style={layoutStyle}>
      <a
        href="#main-content"
        className="pool-skip-link"
        onClick={() => document.getElementById('main-content')?.focus()}
        inert={isAdmin && responsive.isMobile && mobileOpen ? true : undefined}
        tabIndex={isAdmin && responsive.isMobile && mobileOpen ? -1 : undefined}
        aria-hidden={isAdmin && responsive.isMobile && mobileOpen ? true : undefined}
      >
        {t('app.skip_to_content')}
      </a>
      {isAdmin && responsive.isMobile && mobileOpen ? <div className="pool-shell-drawer-overlay" aria-hidden="true" onClick={closeMobileMenu} /> : null}
      {isAdmin ? <Sider
        id={responsive.isMobile ? 'pool-mobile-navigation' : 'pool-desktop-navigation'}
        inert={responsive.isMobile && !mobileOpen ? true : undefined}
        aria-hidden={responsive.isMobile ? !mobileOpen : undefined}
        aria-label={responsive.isMobile ? t('app.navigation') : t('app.admin')}
        aria-modal={responsive.isMobile && mobileOpen ? true : undefined}
        role={responsive.isMobile ? 'dialog' : undefined}
        className={`pool-shell-sider ${navCollapsed ? 'pool-sider-collapsed' : 'pool-sider-expanded'}`}
        style={{ width: sidebarWidth, transform: responsive.isMobile ? (mobileOpen ? 'translateX(0)' : 'translateX(-100%)') : undefined }}
      >
        <div className="pool-brand">
          <Avatar size="extra-small">P</Avatar>{!navCollapsed ? <span>{t('app.title')}</span> : null}
          {responsive.isMobile ? <Button className="pool-mobile-drawer-close" theme="borderless" icon={<IconClose />} onClick={closeMobileMenu} aria-label={t('common.close')} /> : null}
        </div>
        <Nav
          selectedKeys={[currentNavKey]}
          items={navigation}
          isCollapsed={navCollapsed}
          onClick={({ itemKey, group }: { itemKey: string; group?: boolean }) => {
            if (group && navCollapsed && !responsive.isMobile) { setCollapsed(false); return; }
            if (itemKey?.startsWith('/')) navigateFromShell(itemKey);
          }}
          className="pool-nav-scroll"
          key={`${locale}:${currentNavKey}:${navCollapsed}`}
        />
        {!responsive.isMobile ? (
          <div className="pool-sidebar-collapse"><Button theme="borderless" icon={<IconList />} onClick={() => setCollapsed((value) => !value)} aria-label={navCollapsed ? t('app.expand_sidebar') : t('app.collapse_sidebar')}>{!navCollapsed ? t('app.collapse_sidebar') : null}</Button></div>
        ) : null}
      </Sider> : null}
      <Layout
        className="pool-main-layout"
        inert={isAdmin && responsive.isMobile && mobileOpen ? true : undefined}
        aria-hidden={isAdmin && responsive.isMobile && mobileOpen ? true : undefined}
        style={{ marginLeft: isAdmin && !responsive.isMobile ? sidebarWidth : 0 }}
      >
        <RouteStatus announcement={routeAnnouncement} />
        <Header className={`pool-shell-header ${isAdmin ? 'pool-admin-header' : 'pool-portal-header'}`}>
          {isAdmin ? <div className="pool-topbar-left">
            {responsive.isMobile ? <Button ref={mobileMenuButtonRef} theme="borderless" icon={<IconList />} onClick={() => { setAccountMenuOpen(false); setMobileOpen((value) => !value); }} className="pool-mobile-menu-btn" aria-label={t('app.toggle_menu')} aria-expanded={mobileOpen} aria-controls="pool-mobile-navigation" /> : null}
            <div className="pool-topbar-title"><span className="pool-topbar-title-main">{t('app.admin')}</span>{activeRoute ? <><span className="pool-topbar-divider">/</span><span className="pool-topbar-current">{t(activeRoute.titleKey)}</span></> : null}</div>
          </div> : <>
            <button type="button" className="pool-portal-brand" onClick={() => navigateFromShell('/portal')} aria-label={t('app.portal')}>
              <Avatar size="extra-small">P</Avatar><span>{t('app.portal')}</span>
            </button>
            {!responsive.isMobile ? <nav className="pool-portal-nav" aria-label={t('app.navigation')}>
              {navigation.map((item) => <button type="button" key={item.itemKey} className="pool-portal-nav__item" aria-label={item.text} aria-current={currentNavKey === item.itemKey ? 'page' : undefined} onClick={() => navigateFromShell(item.itemKey)}>{item.icon}<span>{item.text}</span></button>)}
            </nav> : <div className="pool-portal-mobile-title">{activeRoute ? t(activeRoute.titleKey) : t('app.portal')}</div>}
          </>}
          <div className="pool-topbar-actions">
            <Button className="pool-topbar-icon-button pool-desktop-only" theme="borderless" icon={<IconLanguage />} onClick={switchLocale} aria-label={t('app.language')} />
            <Button className="pool-topbar-icon-button pool-desktop-only" theme="borderless" icon={theme.resolved === 'dark' ? <IconMoon /> : <IconSun />} onClick={theme.cycle} aria-label={`${t('app.theme')}: ${t(`theme.${theme.preference}`)}`} title={`${t('app.theme')}: ${t(`theme.${theme.preference}`)}`} />
            {accountMenu}
          </div>
        </Header>
        <Content id="main-content" tabIndex={-1} className="pool-content"><div className="pool-shell"><AppRoutes admin={isAdmin} routeIdentity={routeIdentity} routeTitle={routeTitle} onViewCommit={handleViewCommit} /></div></Content>
        {!isAdmin && responsive.isMobile ? <nav className="pool-portal-tabbar" aria-label={t('app.navigation')}>
          {navigation.map((item) => <button type="button" key={item.itemKey} className="pool-portal-tabbar__item" aria-label={item.text} aria-current={currentNavKey === item.itemKey ? 'page' : undefined} onClick={() => navigateFromShell(item.itemKey)}>{item.icon}<span>{item.text}</span></button>)}
        </nav> : null}
      </Layout>
    </Layout>
  );
}
