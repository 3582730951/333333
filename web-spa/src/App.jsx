import React, { Suspense, lazy, useState, useEffect, useCallback, useMemo, useRef } from 'react';
import { Routes, Route, useNavigate, useLocation, Navigate } from 'react-router-dom';
import { Layout, Nav, Button, Avatar, Toast, Spin } from '@douyinfe/semi-ui';
import {
  IconHome, IconUserGroup, IconSetting, IconHistogram, IconKey, IconMoon, IconSun,
  IconExit, IconPulse, IconList, IconLanguage, IconChevronDown,
} from '@douyinfe/semi-icons';
import { clearToken, me, logout as apiLogout, isUnauthorizedError } from './api.js';
import AppErrorBoundary, {
  isChunkLoadError,
  notifyChunkUpdateAvailable,
  reportClientError,
} from './components/AppErrorBoundary.jsx';
import LoadErrorBanner from './components/LoadErrorBanner.jsx';
import useResponsiveLayout from './hooks/useResponsiveLayout.js';
import { setDocumentBodyAttribute } from './lib/browserDocument.js';
import { addDocumentListener, addWindowListener, cancelBrowserIdleCallback, requestBrowserIdleCallback } from './lib/browserLifecycle.js';
import { prefersReducedNetworkData } from './lib/browserNetwork.js';
import { getLocalItem, setLocalItem } from './lib/browserStorage.js';
import { t, getLocale, setLocale } from './lib/i18n.js';

const { Header, Sider, Content } = Layout;
const SIDEBAR_EXPANDED_WIDTH = 222;
const SIDEBAR_COLLAPSED_WIDTH = 60;

const pageLoaders = {
  Login: () => import('./pages/Login.jsx'),
  Dashboard: () => import('./pages/Dashboard.jsx'),
  Accounts: () => import('./pages/Accounts.jsx'),
  Groups: () => import('./pages/Groups.jsx'),
  Egress: () => import('./pages/Egress.jsx'),
  Registration: () => import('./pages/Registration.jsx'),
  Usage: () => import('./pages/Usage.jsx'),
  SettingsV2: () => import('./pages/SettingsV2.jsx'),
  Keys: () => import('./pages/Keys.jsx'),
  Providers: () => import('./pages/Providers.jsx'),
  Lifecycle: () => import('./pages/Lifecycle.jsx'),
  Quota: () => import('./pages/Quota.jsx'),
  CFEvents: () => import('./pages/CFEvents.jsx'),
  Audit: () => import('./pages/Audit.jsx'),
  Users: () => import('./pages/Users.jsx'),
  Thinking: () => import('./pages/Thinking.jsx'),
  Moderation: () => import('./pages/Moderation.jsx'),
  Gopay: () => import('./pages/Gopay.jsx'),
  System: () => import('./pages/System.jsx'),
  PortalDashboard: () => import('./pages/portal/PortalDashboard.jsx'),
  PortalKeys: () => import('./pages/portal/PortalKeys.jsx'),
  PortalProfile: () => import('./pages/portal/PortalProfile.jsx'),
};

const Login = lazy(pageLoaders.Login);
const Dashboard = lazy(pageLoaders.Dashboard);
const Accounts = lazy(pageLoaders.Accounts);
const Groups = lazy(pageLoaders.Groups);
const Egress = lazy(pageLoaders.Egress);
const Registration = lazy(pageLoaders.Registration);
const Usage = lazy(pageLoaders.Usage);
const SettingsV2 = lazy(pageLoaders.SettingsV2);
const Keys = lazy(pageLoaders.Keys);
const Providers = lazy(pageLoaders.Providers);
const Lifecycle = lazy(pageLoaders.Lifecycle);
const Quota = lazy(pageLoaders.Quota);
const CFEvents = lazy(pageLoaders.CFEvents);
const Audit = lazy(pageLoaders.Audit);
const Users = lazy(pageLoaders.Users);
const Thinking = lazy(pageLoaders.Thinking);
const Moderation = lazy(pageLoaders.Moderation);
const Gopay = lazy(pageLoaders.Gopay);
const System = lazy(pageLoaders.System);
const PortalDashboard = lazy(pageLoaders.PortalDashboard);
const PortalKeys = lazy(pageLoaders.PortalKeys);
const PortalProfile = lazy(pageLoaders.PortalProfile);

const ADMIN_PREFETCH_LOADERS = [
  { name: 'Accounts', load: pageLoaders.Accounts },
  { name: 'Usage', load: pageLoaders.Usage },
  { name: 'Keys', load: pageLoaders.Keys },
  { name: 'SettingsV2', load: pageLoaders.SettingsV2 },
  { name: 'System', load: pageLoaders.System },
];
const PORTAL_PREFETCH_LOADERS = [
  { name: 'PortalKeys', load: pageLoaders.PortalKeys },
  { name: 'PortalProfile', load: pageLoaders.PortalProfile },
];

function reportRoutePrefetchError(error, routeName) {
  reportClientError(error, {
    source: 'route.prefetch',
    componentStack: routeName ? `prefetch:${routeName}` : '',
  });
  if (isChunkLoadError(error)) {
    notifyChunkUpdateAvailable(error);
  }
  if (import.meta.env.DEV) console.debug('[prefetch] route module failed', routeName, error);
}

function PageFallback() {
  return (
    <div style={{ minHeight: 260, display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
      <Spin size="large" />
    </div>
  );
}

const ADMIN_ROUTE_MODEL = [
  { path: '/', labelKey: 'nav.dashboard', icon: <IconHome />, component: Dashboard },
  { key: 'g-acc', labelKey: 'nav.accounts', icon: <IconUserGroup />, children: [
    { path: '/accounts', labelKey: 'nav.account_pool', component: Accounts },
    { path: '/groups', labelKey: 'nav.groups', component: Groups },
    { path: '/egress', labelKey: 'nav.egress', component: Egress },
    { path: '/providers', labelKey: 'nav.providers', component: Providers },
  ] },
  { key: 'g-reg', labelKey: 'nav.registration', icon: <IconPulse />, children: [
    { path: '/registration', labelKey: 'nav.auto_register', component: Registration },
    { navPath: '/settings-v2#automation', labelKey: 'nav.automation' },
    { path: '/lifecycle', labelKey: 'nav.lifecycle', component: Lifecycle },
    { path: '/gopay', labelKey: 'nav.gopay', component: Gopay },
  ] },
  { key: 'g-mon', labelKey: 'nav.monitor', icon: <IconHistogram />, children: [
    { path: '/usage', labelKey: 'nav.usage', component: Usage },
    { path: '/quota', labelKey: 'nav.quota', component: Quota },
    { path: '/system', labelKey: 'nav.system', component: System },
    { path: '/cf-events', labelKey: 'nav.cf_events', component: CFEvents },
    { path: '/audit', labelKey: 'nav.audit', component: Audit },
  ] },
  { key: 'g-sys', labelKey: 'nav.sys', icon: <IconSetting />, children: [
    { path: '/keys', labelKey: 'nav.keys', component: Keys },
    { path: '/users', labelKey: 'nav.users', component: Users },
    { path: '/thinking', labelKey: 'nav.thinking', component: Thinking },
    { path: '/moderation', labelKey: 'nav.moderation', component: Moderation },
    { path: '/settings-v2', labelKey: 'nav.settings', component: SettingsV2 },
  ] },
];

const ADMIN_EXTRA_ROUTES = [
  { path: '/automation', redirectTo: '/settings-v2#automation' },
  { path: '/settings', redirectTo: '/settings-v2' },
];

const PORTAL_ROUTE_MODEL = [
  { path: '/portal', labelKey: 'nav.my_usage', icon: <IconHistogram />, component: PortalDashboard },
  { path: '/portal/keys', labelKey: 'nav.my_keys', icon: <IconKey />, component: PortalKeys },
  { path: '/portal/profile', labelKey: 'nav.my_profile', icon: <IconSetting />, component: PortalProfile },
];

function routeNavKey(route) {
  return route.navPath || route.path || route.key;
}

function buildNavItems(routes) {
  return routes.map((route) => {
    if (route.children) {
      return {
        itemKey: route.key,
        text: t(route.labelKey),
        icon: route.icon,
        items: buildNavItems(route.children),
      };
    }
    return {
      itemKey: routeNavKey(route),
      text: t(route.labelKey),
      icon: route.icon,
    };
  });
}

function buildLabelMapFromNav(items) {
  const map = { '/settings': t('nav.settings') };
  const visit = (item) => {
    if (item.itemKey?.startsWith('/')) map[item.itemKey] = item.text;
    item.items?.forEach(visit);
  };
  items.forEach(visit);
  return map;
}

function flattenRouteEntries(routes) {
  const out = [];
  const visit = (route) => {
    route.children?.forEach(visit);
    if (route.path && (route.component || route.redirectTo)) out.push(route);
  };
  routes.forEach(visit);
  return out;
}

const adminRouteEntries = [...flattenRouteEntries(ADMIN_ROUTE_MODEL), ...ADMIN_EXTRA_ROUTES];
const portalRouteEntries = flattenRouteEntries(PORTAL_ROUTE_MODEL);

function routeElement(route) {
  if (route.redirectTo) return <Navigate to={route.redirectTo} replace />;
  const Page = route.component;
  return <Page />;
}

function renderRoutes(routes, fallbackPath) {
  return (
    <Routes>
      {routes.map((route) => <Route key={route.path} path={route.path} element={routeElement(route)} />)}
      <Route path="*" element={<Navigate to={fallbackPath} replace />} />
    </Routes>
  );
}

export default function App() {
  const [auth, setAuth] = useState({ ready: false, authed: false, role: '', user: null, error: null });
  const [dark, setDark] = useState(() => getLocalItem('pool_theme') === 'dark');
  const [locale, setLocaleState] = useState(getLocale());
  const responsive = useResponsiveLayout();
  const [collapsed, setCollapsed] = useState(responsive.collapsedByWidth);
  const [mobileOpen, setMobileOpen] = useState(false);
  const [accountMenuOpen, setAccountMenuOpen] = useState(false);
  const accountMenuRef = useRef(null);
  const navigate = useNavigate();
  const location = useLocation();
  const isAdmin = auth.role === 'admin';

  // Listen for locale change from other tabs or the i18n module.
  useEffect(() => {
    const handler = (e) => setLocaleState(e.detail);
    return addWindowListener('pool-locale-change', handler);
  }, []);

  useEffect(() => {
    setCollapsed(responsive.collapsedByWidth);
    if (!responsive.isMobile && mobileOpen) setMobileOpen(false);
  }, [responsive.collapsedByWidth, responsive.isMobile, mobileOpen]);

  const checkAuth = useCallback(async () => {
    setAuth((prev) => ({ ...prev, ready: false, error: null }));
    try {
      const r = await me({ suppressUnauthorizedEvent: true });
      if (r && (r.authed || r.via === 'open')) {
        setAuth({ ready: true, authed: true, role: r.role || 'user', user: r, error: null });
        return;
      }
    } catch (error) {
      if (!isUnauthorizedError(error)) {
        setAuth({ ready: true, authed: false, role: '', user: null, error });
        return;
      }
    }
    setAuth({ ready: true, authed: false, role: '', user: null, error: null });
  }, []);

  useEffect(() => { checkAuth(); }, [checkAuth]);

  useEffect(() => {
    if (!auth.ready || !auth.authed) return undefined;
    if (prefersReducedNetworkData()) return undefined;

    let cancelled = false;
    const prefetch = () => {
      if (cancelled) return;
      const loaders = isAdmin ? ADMIN_PREFETCH_LOADERS : PORTAL_PREFETCH_LOADERS;
      loaders.forEach(({ load, name }) => {
        load().catch((err) => {
          reportRoutePrefetchError(err, name);
        });
      });
    };
    const idleID = requestBrowserIdleCallback(prefetch, { timeout: 3500 });
    return () => {
      cancelled = true;
      cancelBrowserIdleCallback(idleID);
    };
  }, [auth.ready, auth.authed, isAdmin]);

  useEffect(() => {
    if (!accountMenuOpen) return undefined;
    const onPointerDown = (event) => {
      if (accountMenuRef.current?.contains(event.target)) return;
      setAccountMenuOpen(false);
    };
    const onKeyDown = (event) => {
      if (event.key === 'Escape') setAccountMenuOpen(false);
    };
    const removePointer = addDocumentListener('pointerdown', onPointerDown);
    const removeKey = addDocumentListener('keydown', onKeyDown);
    return () => {
      removePointer();
      removeKey();
    };
  }, [accountMenuOpen]);

  useEffect(() => {
    setDocumentBodyAttribute('theme-mode', dark ? 'dark' : null);
    setLocalItem('pool_theme', dark ? 'dark' : 'light');
  }, [dark]);

  useEffect(() => {
    const onUnauth = () => { clearToken(); setAuth({ ready: true, authed: false, role: '', user: null, error: null }); Toast.error('登录已失效，请重新登录'); };
    return addWindowListener('pool-unauthorized', onUnauth);
  }, []);

  const logout = useCallback(async () => {
    try { await apiLogout(); } catch { /* ignore */ }
    clearToken();
    setAuth({ ready: true, authed: false, role: '', user: null, error: null });
    navigate('/');
  }, [navigate]);

  const nav = useMemo(() => buildNavItems(isAdmin ? ADMIN_ROUTE_MODEL : PORTAL_ROUTE_MODEL), [isAdmin, locale]);
  const ident = auth.user?.email || auth.user?.name || (isAdmin ? 'admin' : 'user');
  const identInitial = String(ident || 'U').trim().charAt(0).toUpperCase();
  const currentNavKey = location.pathname === '/settings-v2' && location.hash ? `${location.pathname}${location.hash}` : location.pathname;
  const labelMap = useMemo(() => buildLabelMapFromNav(nav), [nav, locale]);
  const pageLabel = labelMap[currentNavKey] || labelMap[location.pathname] || '';
  const navCollapsed = responsive.isMobile ? false : collapsed;
  const sidebarWidth = responsive.isMobile
    ? SIDEBAR_EXPANDED_WIDTH
    : navCollapsed
      ? SIDEBAR_COLLAPSED_WIDTH
      : SIDEBAR_EXPANDED_WIDTH;
  const contentOffset = responsive.isMobile ? 0 : sidebarWidth;
  const sidebarWidthPx = `${sidebarWidth}px`;
  const contentOffsetPx = `${contentOffset}px`;

  if (!auth.ready) {
    return <div style={{ height: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center' }}><Spin size="large" /></div>;
  }
  if (auth.error) {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24, background: 'var(--semi-color-bg-0)' }}>
        <div style={{ width: 'min(560px, 100%)' }}>
          <LoadErrorBanner error={auth.error} onRetry={checkAuth} title="控制台连接失败" />
        </div>
      </div>
    );
  }
  if (!auth.authed) {
    return (
      <Suspense fallback={<PageFallback />}>
        <Login onSuccess={checkAuth} />
      </Suspense>
    );
  }

  return (
    <Layout
      className={[
        'pool-app-layout',
        responsive.isMobile ? 'pool-app-mobile' : 'pool-app-desktop',
        navCollapsed ? 'pool-sidebar-is-collapsed' : 'pool-sidebar-is-expanded',
      ].join(' ')}
      style={{ height: '100vh', '--pool-sidebar-width': sidebarWidthPx }}
    >
      {/* Mobile overlay */}
      {mobileOpen && (
        <div style={{
          position: 'fixed', inset: 0, background: 'rgba(0,0,0,0.6)', zIndex: 999,
          backdropFilter: 'blur(4px)', transition: 'opacity 0.3s ease'
        }} onClick={() => setMobileOpen(false)} />
      )}
      <Sider
        style={{
          width: sidebarWidthPx,
          minWidth: sidebarWidthPx,
          maxWidth: sidebarWidthPx,
          flex: `0 0 ${sidebarWidthPx}`,
          overflow: 'hidden',
          background: 'var(--pool-bg-surface)',
          borderRight: '1px solid var(--pool-border)',
          position: 'fixed',
          height: '100vh',
          zIndex: 1000,
          transition: responsive.isMobile ? 'transform 0.3s ease' : 'none',
          transform: mobileOpen ? 'translateX(0)' : undefined,
        }}
        className={[
          responsive.isMobile && !mobileOpen ? 'sider-collapsed-mobile' : '',
          navCollapsed ? 'pool-sider-collapsed' : 'pool-sider-expanded',
        ].filter(Boolean).join(' ')}
      >
        <div className="pool-brand" style={{ height: 56, borderBottom: '1px solid var(--pool-border)' }}>
          <Avatar size="extra-small" style={{ background: 'var(--semi-color-primary)' }}>P</Avatar>
          {!navCollapsed && <span style={{ marginLeft: 10 }}>{t('app.title')}</span>}
        </div>
        <Nav
          selectedKeys={[currentNavKey]}
          defaultOpenKeys={['g-acc', 'g-reg', 'g-mon']}
          items={nav}
          isCollapsed={navCollapsed}
          onCollapseChange={(nextCollapsed) => {
            if (responsive.isMobile) return;
            setCollapsed(Boolean(nextCollapsed));
          }}
          onClick={({ itemKey }) => { if (typeof itemKey === 'string' && itemKey.startsWith('/')) { navigate(itemKey); setMobileOpen(false); } }}
          className="pool-nav-scroll"
          key={`${locale}:${currentNavKey}:${navCollapsed ? 'collapsed' : 'expanded'}`}
          style={{
            width: sidebarWidthPx,
            minWidth: sidebarWidthPx,
            maxWidth: sidebarWidthPx,
            height: responsive.isMobile ? 'calc(100vh - 56px)' : 'calc(100vh - 104px)',
            overflowY: 'auto',
          }}
        />
        {!responsive.isMobile ? (
          <div className="pool-sidebar-collapse">
            <Button
              theme="borderless"
              icon={<IconList />}
              onClick={() => setCollapsed((value) => !value)}
              aria-label={navCollapsed ? '展开侧边栏' : '收起侧边栏'}
            >
              {!navCollapsed ? '收起侧边栏' : null}
            </Button>
          </div>
        ) : null}
      </Sider>
      <Layout className="pool-main-layout" style={{ marginLeft: contentOffsetPx, transition: 'none' }}>
        <Header style={{
          background: 'var(--pool-bg-surface)',
          borderBottom: '1px solid var(--pool-border)',
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          padding: '0 20px',
          height: 56,
          position: 'sticky',
          top: 0,
          zIndex: 100,
          gap: 16
        }}>
          {/* Mobile menu toggle */}
          <div className="pool-topbar-left" style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
            <Button
              theme="borderless"
              icon={<IconList />}
              onClick={() => setMobileOpen(!mobileOpen)}
              className="mobile-menu-btn"
              style={{ display: responsive.isMobile ? 'flex' : 'none' }}
              aria-label="切换菜单"
            />
            <div className="pool-topbar-title" style={{ fontWeight: 600, color: 'var(--semi-color-text-1)', display: 'flex', alignItems: 'center', gap: 8 }}>
              <span className="pool-topbar-title-main">{isAdmin ? t('app.admin') : t('app.portal')}</span>
              {pageLabel ? (
                <>
                  <span className="pool-topbar-divider" style={{ color: 'var(--semi-color-text-3)' }}>·</span>
                  <span className="pool-topbar-current" style={{ color: 'var(--semi-color-text-2)', fontWeight: 400 }}>{pageLabel}</span>
                </>
              ) : null}
            </div>
          </div>
          <div className="pool-topbar-actions" style={{ display: 'flex', alignItems: 'center', gap: 8, flexShrink: 0 }}>
            <span className="pool-topbar-tooltip" data-tooltip={locale === 'en' ? '切换到中文' : 'Switch to English'}>
              <Button
                className="pool-topbar-icon-button"
                theme="borderless"
                icon={<IconLanguage />}
                onClick={() => { const l = locale === 'en' ? 'zh' : 'en'; setLocale(l); setLocaleState(l); }}
                aria-label={t('app.language')}
                title={locale === 'en' ? '切换到中文' : 'Switch to English'}
              />
            </span>
            <span className="pool-topbar-tooltip" data-tooltip={dark ? '切换浅色模式' : '切换深色模式'}>
              <Button
                className="pool-topbar-icon-button"
                theme="borderless"
                icon={dark ? <IconSun /> : <IconMoon />}
                onClick={() => setDark((d) => !d)}
                aria-label={t('app.theme')}
                title={dark ? '切换浅色模式' : '切换深色模式'}
              />
            </span>
            <div className="pool-account-menu-wrap" ref={accountMenuRef}>
              <Button
                className="pool-account-menu-button"
                theme="borderless"
                aria-label="账户菜单"
                aria-haspopup="menu"
                aria-expanded={accountMenuOpen}
                onClick={() => setAccountMenuOpen((value) => !value)}
              >
                <Avatar size="extra-small" className="pool-account-avatar">{identInitial}</Avatar>
                <span className="pool-account-menu-ident">{ident}</span>
                <IconChevronDown className="pool-account-menu-caret" />
              </Button>
              {accountMenuOpen ? (
                <div className="pool-account-menu" role="menu">
                  <div className="pool-account-menu-title">
                    <span>{ident}</span>
                    <small>{isAdmin ? '管理员' : '用户'}</small>
                  </div>
                  <div className="pool-account-menu-divider" />
                  <button
                    type="button"
                    className="pool-account-menu-item"
                    role="menuitem"
                    onClick={() => {
                      setAccountMenuOpen(false);
                      logout();
                    }}
                  >
                    <IconExit />
                    <span>{t('app.logout')}</span>
                  </button>
                </div>
              ) : null}
            </div>
          </div>
        </Header>
        <Content className="pool-content" style={{ overflow: 'auto', background: 'var(--pool-bg-page)' }}>
          <div className="pool-shell">
            <AppErrorBoundary
              variant="page"
              resetKey={`${location.pathname}${location.hash}`}
              onHome={() => navigate(isAdmin ? '/' : '/portal')}
            >
              <Suspense fallback={<PageFallback />}>
                {renderRoutes(isAdmin ? adminRouteEntries : portalRouteEntries, isAdmin ? '/' : '/portal')}
              </Suspense>
            </AppErrorBoundary>
          </div>
        </Content>
      </Layout>
    </Layout>
  );
}
