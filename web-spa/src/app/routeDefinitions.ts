import type { RouteDefinition } from '../model/contracts';

export const adminRoutes = [
  { path: '/', role: 'admin', navGroup: 'overview', titleKey: 'nav.dashboard', descriptionKey: 'page.dashboard.desc', lazyLoader: () => import('../pages/Dashboard.tsx'), prefetch: 'eager' },
  { path: '/accounts', role: 'admin', navGroup: 'accounts', titleKey: 'nav.account_pool', descriptionKey: 'page.accounts.desc', lazyLoader: () => import('../pages/Accounts.jsx'), prefetch: 'eager' },
  { path: '/groups', role: 'admin', navGroup: 'accounts', titleKey: 'nav.groups', descriptionKey: 'page.groups.desc', lazyLoader: () => import('../pages/Groups.jsx') },
  { path: '/providers', role: 'admin', navGroup: 'access', titleKey: 'nav.providers', descriptionKey: 'page.providers.desc', lazyLoader: () => import('../pages/Providers.jsx') },
  { path: '/models', role: 'admin', navGroup: 'access', titleKey: 'nav.models', descriptionKey: 'page.models.desc', lazyLoader: () => import('../pages/Models.tsx') },
  { path: '/egress', role: 'admin', navGroup: 'access', titleKey: 'nav.egress', descriptionKey: 'page.egress.desc', lazyLoader: () => import('../pages/Egress.jsx'), prefetch: 'idle' },
  { path: '/upstream-error-rules', role: 'admin', navGroup: 'access', titleKey: 'nav.upstream_error_rules', descriptionKey: 'page.upstream.desc', lazyLoader: () => import('../pages/UpstreamErrorRules.jsx') },
  { path: '/registration', role: 'admin', navGroup: 'automation', titleKey: 'nav.auto_register', descriptionKey: 'page.registration.desc', lazyLoader: () => import('../pages/Registration.tsx'), prefetch: 'idle' },
  { path: '/lifecycle', role: 'admin', navGroup: 'automation', titleKey: 'nav.lifecycle', descriptionKey: 'page.lifecycle.desc', lazyLoader: () => import('../pages/Lifecycle.tsx') },
  { path: '/gopay', role: 'admin', navGroup: 'automation', titleKey: 'nav.gopay', descriptionKey: 'page.gopay.desc', lazyLoader: () => import('../pages/Gopay.jsx') },
  { path: '/usage', role: 'admin', navGroup: 'observability', titleKey: 'nav.usage', descriptionKey: 'page.usage.desc', lazyLoader: () => import('../pages/Usage.tsx'), prefetch: 'eager' },
  { path: '/quota', role: 'admin', navGroup: 'observability', titleKey: 'nav.quota', descriptionKey: 'page.quota.desc', lazyLoader: () => import('../pages/Quota.tsx') },
  { path: '/model-quality', role: 'admin', navGroup: 'observability', titleKey: 'nav.model_quality', descriptionKey: 'page.quality.desc', lazyLoader: () => import('../pages/ModelQuality.jsx') },
  { path: '/system', role: 'admin', navGroup: 'observability', titleKey: 'nav.system', descriptionKey: 'page.system.desc', lazyLoader: () => import('../pages/System.tsx'), prefetch: 'eager' },
  { path: '/cf-events', role: 'admin', navGroup: 'observability', titleKey: 'nav.cf_events', descriptionKey: 'page.cf.desc', lazyLoader: () => import('../pages/CFEvents.tsx') },
  { path: '/audit', role: 'admin', navGroup: 'observability', titleKey: 'nav.audit', descriptionKey: 'page.audit.desc', lazyLoader: () => import('../pages/Audit.tsx') },
  { path: '/keys', role: 'admin', navGroup: 'security', titleKey: 'nav.keys', descriptionKey: 'page.keys.desc', lazyLoader: () => import('../pages/Keys.tsx'), prefetch: 'eager' },
  { path: '/users', role: 'admin', navGroup: 'security', titleKey: 'nav.users', descriptionKey: 'page.users.desc', lazyLoader: () => import('../pages/Users.tsx') },
  { path: '/settings-v2', role: 'admin', navGroup: 'settings', titleKey: 'nav.settings', descriptionKey: 'page.settings.desc', lazyLoader: () => import('../pages/SettingsV2.tsx'), prefetch: 'eager' },
] as const satisfies ReadonlyArray<RouteDefinition>;

export const portalRoutes = [
  { path: '/portal', role: 'user', navGroup: 'portal', titleKey: 'nav.my_usage', descriptionKey: 'page.portal_usage.desc', lazyLoader: () => import('../pages/portal/PortalDashboard.tsx'), prefetch: 'eager' },
  { path: '/portal/keys', role: 'user', navGroup: 'portal', titleKey: 'nav.my_keys', descriptionKey: 'page.portal_keys.desc', lazyLoader: () => import('../pages/portal/PortalKeys.tsx'), prefetch: 'idle' },
  { path: '/portal/models', role: 'user', navGroup: 'portal', titleKey: 'nav.models', descriptionKey: 'page.models.desc', lazyLoader: () => import('../pages/portal/PortalModels.tsx'), prefetch: 'idle' },
  { path: '/portal/profile', role: 'user', navGroup: 'portal', titleKey: 'nav.my_profile', descriptionKey: 'page.portal_profile.desc', lazyLoader: () => import('../pages/portal/PortalProfile.jsx'), prefetch: 'idle' },
] as const satisfies ReadonlyArray<RouteDefinition>;

export const legacyRedirects = [
  { path: '/settings', to: '/settings-v2?tab=config' },
  { path: '/automation', to: '/settings-v2?tab=automation' },
  { path: '/thinking', to: '/settings-v2?tab=thinking' },
  { path: '/moderation', to: '/settings-v2?tab=moderation' },
] as const;

export const settingsSections = [
  { key: 'config', labelKey: 'settings.general' },
  { key: 'automation', labelKey: 'settings.automation' },
  { key: 'registrar', labelKey: 'settings.registrar' },
  { key: 'lifecycle', labelKey: 'settings.lifecycle' },
  { key: 'logging', labelKey: 'settings.logging' },
  { key: 'memory', labelKey: 'settings.memory' },
  { key: 'thinking', labelKey: 'settings.thinking' },
  { key: 'moderation', labelKey: 'settings.moderation' },
] as const;

export const adminVisualRoutes = [
  ...adminRoutes.map((route) => ({ name: route.titleKey, path: route.path })),
  { name: 'settings.thinking', path: '/thinking' },
  { name: 'settings.moderation', path: '/moderation' },
] as const;

export const portalVisualRoutes = portalRoutes.map((route) => ({ name: route.titleKey, path: route.path }));
