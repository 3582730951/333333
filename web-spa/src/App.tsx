import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState, type ComponentType, type CSSProperties, type LazyExoticComponent, type ReactNode } from 'react';
import { Navigate, Route, Routes, useLocation, useNavigate } from 'react-router';
import { useIsFetching } from '@tanstack/react-query';
import * as PoolUI from './components/pool/index.jsx';
import {
  IconChevronDown, IconExit, IconHistogram, IconHome, IconKey, IconLanguage,
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

function BootScreen() {
  return (
    <div className="pool-boot-shell" data-page-ready="false">
      <aside className="pool-boot-sidebar">
        <div className="pool-boot-brand"><span className="pool-skel" /><span className="pool-skel" /></div>
        {Array.from({ length: 7 }).map((_, index) => <div className="pool-skel pool-boot-nav" key={index} />)}
      </aside>
      <main className="pool-boot-main">
        <header className="pool-boot-header"><span className="pool-skel" /><span className="pool-skel" /></header>
        <RouteFallback />
      </main>
    </div>
  );
}

function adminNavigation() {
  const overview = adminRoutes.find((route) => route.path === '/');
  const items: any[] = overview ? [{ itemKey: '/', text: t(overview.titleKey), icon: <IconHome /> }] : [];
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

function portalNavigation() {
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

function AppRoutes({ admin }: { admin: boolean }) {
  const location = useLocation();
  const routes: ReadonlyArray<RouteDefinition> = admin ? adminRoutes : portalRoutes;
  const pages = admin ? adminPages : portalPages;
  const fallback = admin ? '/' : '/portal';
  return (
    <AppErrorBoundary variant="page" resetKey={`${location.pathname}${location.search}`}>
      <Suspense fallback={<RouteFallback />}>
        <StablePageReady routeKey={`${location.pathname}${location.search}`}>
          <Routes>
            {routes.map((route) => {
              const Page = pages.get(route.path)!;
              return <Route key={route.path} path={route.path} element={<Page />} />;
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
  const accountMenuRef = useRef<HTMLDivElement | null>(null);
  const isAdmin = auth.role === 'admin';

  useEffect(() => {
    resetDocumentOverlayLocks();
    return resetDocumentOverlayLocks;
  }, [location.pathname, location.search]);

  useEffect(() => addWindowListener('pool-locale-change', (event: CustomEvent<string>) => setLocaleState(event.detail === 'en' ? 'en' : 'zh')), []);
  useEffect(() => addWindowListener('pool-ai-settings-dirty', (event: CustomEvent<boolean>) => setAISettingsDirty(Boolean(event.detail))), []);
  useEffect(() => {
    setCollapsed(responsive.collapsedByWidth);
    if (!responsive.isMobile) setMobileOpen(false);
  }, [responsive.collapsedByWidth, responsive.isMobile]);

  useEffect(() => {
    if (!accountMenuOpen) return undefined;
    const closeOnOutside = (event: PointerEvent) => {
      if (!accountMenuRef.current?.contains(event.target as Node)) setAccountMenuOpen(false);
    };
    const closeOnEscape = (event: KeyboardEvent) => { if (event.key === 'Escape') setAccountMenuOpen(false); };
    const removePointer = addDocumentListener('pointerdown', closeOnOutside);
    const removeKeyboard = addDocumentListener('keydown', closeOnEscape);
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
  const ident = auth.user?.email || auth.user?.name || (isAdmin ? 'admin' : 'user');
  const identInitial = String(ident).trim().charAt(0).toUpperCase();
  const navCollapsed = responsive.isMobile ? false : collapsed;
  const sidebarWidth = responsive.isMobile ? SIDEBAR_EXPANDED_WIDTH : navCollapsed ? SIDEBAR_COLLAPSED_WIDTH : SIDEBAR_EXPANDED_WIDTH;
  const navigateFromShell = (target: string) => {
    if (target === `${location.pathname}${location.search}` || target === location.pathname) return;
    if (aiSettingsDirty && !window.confirm(t('ai_settings.leave_description'))) return;
    setAISettingsDirty(false);
    navigate(target);
    setMobileOpen(false);
  };

  if (!auth.ready) return <BootScreen />;
  if (auth.error) {
    return (
      <div className="pool-auth-error">
        <div className="pool-auth-error__brand"><Avatar size="small">P</Avatar><strong>{t('app.title')}</strong></div>
        <LoadErrorBanner error={auth.error} onRetry={auth.refresh} title={t('error.console_connection')} />
      </div>
    );
  }
  if (!auth.authed) {
    return <Suspense fallback={<BootScreen />}><LoginPage onSuccess={auth.refresh} /></Suspense>;
  }

  const layoutStyle = { '--pool-sidebar-width': `${sidebarWidth}px` } as CSSProperties;
  return (
    <Layout className={`pool-app-layout ${responsive.isMobile ? 'pool-app-mobile' : 'pool-app-desktop'} ${navCollapsed ? 'pool-sidebar-is-collapsed' : ''}`} style={layoutStyle}>
      {mobileOpen ? <button className="pool-shell-drawer-overlay" aria-label={t('common.close')} onClick={() => setMobileOpen(false)} /> : null}
      <Sider className={`pool-shell-sider ${navCollapsed ? 'pool-sider-collapsed' : 'pool-sider-expanded'}`} style={{ width: sidebarWidth, transform: responsive.isMobile ? (mobileOpen ? 'translateX(0)' : 'translateX(-100%)') : undefined }}>
        <div className="pool-brand"><Avatar size="extra-small">P</Avatar>{!navCollapsed ? <span>{t('app.title')}</span> : null}</div>
        <Nav
          selectedKeys={[currentNavKey]}
          defaultOpenKeys={['group:accounts', 'group:access', 'group:automation', 'group:observability', 'group:security', 'group:settings']}
          items={navigation}
          isCollapsed={navCollapsed}
          onCollapseChange={(value: boolean) => { if (!responsive.isMobile) setCollapsed(Boolean(value)); }}
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
      </Sider>
      <Layout className="pool-main-layout" style={{ marginLeft: responsive.isMobile ? 0 : sidebarWidth }}>
        <Header className="pool-shell-header">
          <div className="pool-topbar-left">
            <Button theme="borderless" icon={<IconList />} onClick={() => setMobileOpen((value) => !value)} className="pool-mobile-menu-btn" aria-label={t('app.toggle_menu')} />
            <div className="pool-topbar-title"><span>{isAdmin ? t('app.admin') : t('app.portal')}</span>{activeRoute ? <><span className="pool-topbar-divider">/</span><span className="pool-topbar-current">{t(activeRoute.titleKey)}</span></> : null}</div>
          </div>
          <div className="pool-topbar-actions">
            <Button className="pool-topbar-icon-button pool-desktop-only" theme="borderless" icon={<IconLanguage />} onClick={switchLocale} aria-label={t('app.language')} />
            <Button className="pool-topbar-icon-button pool-desktop-only" theme="borderless" icon={theme.resolved === 'dark' ? <IconMoon /> : <IconSun />} onClick={theme.cycle} aria-label={`${t('app.theme')}: ${t(`theme.${theme.preference}`)}`} title={`${t('app.theme')}: ${t(`theme.${theme.preference}`)}`} />
            <div className="pool-account-menu-wrap" ref={accountMenuRef}>
              <Button className="pool-account-menu-button" theme="borderless" aria-label={t('app.account_menu')} aria-haspopup="menu" aria-expanded={accountMenuOpen} onClick={() => setAccountMenuOpen((value) => !value)}>
                <Avatar size="extra-small" className="pool-account-avatar">{identInitial}</Avatar><span className="pool-account-menu-ident">{ident}</span><IconChevronDown className="pool-account-menu-caret" />
              </Button>
              {accountMenuOpen ? (
                <div className="pool-account-menu" role="menu">
                  <div className="pool-account-menu-title"><span>{ident}</span><small>{isAdmin ? t('role.admin') : t('role.user')}</small></div>
                  <div className="pool-account-menu-divider" />
                  <button type="button" className="pool-account-menu-item" role="menuitem" onClick={() => { switchLocale(); setAccountMenuOpen(false); }}><IconLanguage /><span>{locale === 'en' ? '切换到中文' : 'Switch to English'}</span></button>
                  <button type="button" className="pool-account-menu-item" role="menuitem" onClick={() => { theme.cycle(); setAccountMenuOpen(false); }}>{theme.resolved === 'dark' ? <IconMoon /> : <IconSun />}<span>{t(`theme.${theme.preference}`)}</span></button>
                  <div className="pool-account-menu-divider" />
                  <button type="button" className="pool-account-menu-item" role="menuitem" onClick={async () => { setAccountMenuOpen(false); await auth.logout(); navigate('/'); Toast.success(t('app.logged_out')); }}><IconExit /><span>{t('app.logout')}</span></button>
                </div>
              ) : null}
            </div>
          </div>
        </Header>
        <Content className="pool-content"><div className="pool-shell"><AppRoutes admin={isAdmin} /></div></Content>
      </Layout>
    </Layout>
  );
}
