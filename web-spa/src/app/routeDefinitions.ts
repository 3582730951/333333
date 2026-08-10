import type { RouteDefinition } from '../model/contracts';

export const adminRoutes = [
  { path: '/', role: 'admin', navGroup: 'overview', titleKey: 'nav.dashboard', descriptionKey: 'page.dashboard.desc', lazyLoader: () => import('../pages/Dashboard.tsx'), prefetch: 'eager' },
  { path: '/accounts', role: 'admin', navGroup: 'accounts', titleKey: 'nav.account_pool', descriptionKey: 'page.accounts.desc', lazyLoader: () => import('../pages/Accounts.jsx'), prefetch: 'eager' },
  { path: '/groups', role: 'admin', navGroup: 'accounts', titleKey: 'nav.groups', descriptionKey: 'page.groups.desc', lazyLoader: () => import('../pages/Groups.jsx') },
  { path: '/providers', role: 'admin', navGroup: 'access', titleKey: 'nav.providers', descriptionKey: 'page.providers.desc', lazyLoader: () => import('../pages/Providers.jsx') },
  { path: '/models', role: 'admin', navGroup: 'access', titleKey: 'nav.models', descriptionKey: 'page.models.desc', lazyLoader: () => import('../pages/Models.tsx'), prefetch: 'idle' },
  { path: '/public-chat', role: 'admin', navGroup: 'access', navHidden: true, titleKey: 'nav.public_chat', descriptionKey: 'page.public_chat.desc', lazyLoader: () => import('../pages/PublicChat.jsx') },
  { path: '/egress', role: 'admin', navGroup: 'access', titleKey: 'nav.egress', descriptionKey: 'page.egress.desc', lazyLoader: () => import('../pages/Egress.jsx'), prefetch: 'never' },
  { path: '/upstream-error-rules', role: 'admin', navGroup: 'access', titleKey: 'nav.upstream_error_rules', descriptionKey: 'page.upstream.desc', lazyLoader: () => import('../pages/UpstreamErrorRules.jsx') },
  { path: '/registration', role: 'admin', navGroup: 'automation', titleKey: 'nav.auto_register', descriptionKey: 'page.registration.desc', lazyLoader: () => import('../pages/Registration.tsx'), prefetch: 'never' },
  { path: '/team-lifecycle', role: 'admin', navGroup: 'automation', titleKey: 'nav.team_lifecycle', descriptionKey: 'page.team_lifecycle.desc', lazyLoader: () => import('../pages/TeamLifecycle.tsx'), prefetch: 'never' },
  { path: '/email-pool', role: 'admin', navGroup: 'accounts', titleKey: 'nav.email_pool', descriptionKey: 'page.email_pool.desc', lazyLoader: () => import('../pages/EmailPool.tsx') },
  { path: '/email-pool/cloudflare', role: 'admin', navGroup: 'accounts', navHidden: true, titleKey: 'cf_mail.title', descriptionKey: 'cf_mail.subtitle', lazyLoader: () => import('../pages/CloudflareMailbox.tsx') },
  { path: '/usage', role: 'admin', navGroup: 'observability', titleKey: 'nav.usage', descriptionKey: 'page.usage.desc', lazyLoader: () => import('../pages/Usage.tsx'), prefetch: 'never' },
  { path: '/quota', role: 'admin', navGroup: 'observability', titleKey: 'nav.quota', descriptionKey: 'page.quota.desc', lazyLoader: () => import('../pages/Quota.tsx') },
  { path: '/model-quality', role: 'admin', navGroup: 'observability', titleKey: 'nav.model_quality', descriptionKey: 'page.quality.desc', lazyLoader: () => import('../pages/ModelQuality.jsx') },
  { path: '/system', role: 'admin', navGroup: 'observability', titleKey: 'nav.system', descriptionKey: 'page.system.desc', lazyLoader: () => import('../pages/System.tsx'), prefetch: 'never' },
  { path: '/cf-events', role: 'admin', navGroup: 'observability', titleKey: 'nav.cf_events', descriptionKey: 'page.cf.desc', lazyLoader: () => import('../pages/CFEvents.tsx') },
  { path: '/audit', role: 'admin', navGroup: 'observability', titleKey: 'nav.audit', descriptionKey: 'page.audit.desc', lazyLoader: () => import('../pages/Audit.tsx') },
  { path: '/keys', role: 'admin', navGroup: 'security', titleKey: 'nav.keys', descriptionKey: 'page.keys.desc', lazyLoader: () => import('../pages/Keys.tsx'), prefetch: 'never' },
  { path: '/users', role: 'admin', navGroup: 'security', titleKey: 'nav.users', descriptionKey: 'page.users.desc', lazyLoader: () => import('../pages/Users.tsx') },
  { path: '/settings-v2', role: 'admin', navGroup: 'settings', titleKey: 'nav.settings', descriptionKey: 'page.settings.desc', lazyLoader: () => import('../pages/SettingsV2.tsx'), prefetch: 'never' },
  { path: '/settings/ai/chatgpt', role: 'admin', navGroup: 'ai_settings', titleKey: 'ai_settings.chatgpt', descriptionKey: 'page.ai_settings.desc', lazyLoader: () => import('../pages/AISettings.tsx') },
  { path: '/settings/ai/claude', role: 'admin', navGroup: 'ai_settings', titleKey: 'ai_settings.claude', descriptionKey: 'page.ai_settings.desc', lazyLoader: () => import('../pages/AISettings.tsx') },
  { path: '/settings/ai/kiro', role: 'admin', navGroup: 'ai_settings', titleKey: 'ai_settings.kiro', descriptionKey: 'page.ai_settings.desc', lazyLoader: () => import('../pages/AISettings.tsx') },
  { path: '/settings/ai/antigravity', role: 'admin', navGroup: 'ai_settings', titleKey: 'ai_settings.antigravity', descriptionKey: 'page.ai_settings.desc', lazyLoader: () => import('../pages/AISettings.tsx') },
  { path: '/settings/ai/codex', role: 'admin', navGroup: 'ai_settings', titleKey: 'ai_settings.codex', descriptionKey: 'page.ai_settings.desc', lazyLoader: () => import('../pages/AISettings.tsx') },
  { path: '/settings/ai/claude-code', role: 'admin', navGroup: 'ai_settings', titleKey: 'ai_settings.claude_code', descriptionKey: 'page.ai_settings.desc', lazyLoader: () => import('../pages/AISettings.tsx') },
] as const satisfies ReadonlyArray<RouteDefinition>;

export const portalRoutes = [
  { path: '/portal', role: 'user', navGroup: 'portal', titleKey: 'nav.my_usage', descriptionKey: 'page.portal_usage.desc', lazyLoader: () => import('../pages/portal/PortalDashboard.tsx'), prefetch: 'eager' },
  { path: '/portal/keys', role: 'user', navGroup: 'portal', titleKey: 'nav.my_keys', descriptionKey: 'page.portal_keys.desc', lazyLoader: () => import('../pages/portal/PortalKeys.tsx'), prefetch: 'never' },
  { path: '/portal/models', role: 'user', navGroup: 'portal', titleKey: 'nav.models', descriptionKey: 'page.models.desc', lazyLoader: () => import('../pages/portal/PortalModels.tsx'), prefetch: 'never' },
  { path: '/portal/profile', role: 'user', navGroup: 'portal', titleKey: 'nav.my_profile', descriptionKey: 'page.portal_profile.desc', lazyLoader: () => import('../pages/portal/PortalProfile.jsx'), prefetch: 'never' },
] as const satisfies ReadonlyArray<RouteDefinition>;

export const legacyRedirects = [
  { path: '/settings', to: '/settings-v2?tab=config' },
  { path: '/automation', to: '/settings-v2?tab=automation' },
  { path: '/thinking', to: '/settings-v2?tab=thinking' },
  { path: '/moderation', to: '/settings-v2?tab=moderation' },
  { path: '/settings/ai', to: '/settings/ai/chatgpt' },
  { path: '/ai-settings', to: '/settings/ai/chatgpt' },
  { path: '/model-settings', to: '/settings/ai/chatgpt' },
  { path: '/settings/models', to: '/settings/ai/chatgpt' },
  { path: '/settings/models/chatgpt', to: '/settings/ai/chatgpt' },
  { path: '/settings/models/claude', to: '/settings/ai/claude' },
  { path: '/settings/models/kiro', to: '/settings/ai/kiro' },
  { path: '/settings/models/antigravity', to: '/settings/ai/antigravity' },
  { path: '/settings/models/codex', to: '/settings/ai/codex' },
  { path: '/settings/models/claude-code', to: '/settings/ai/claude-code' },
  { path: '/super-instruct', to: '/groups' },
] as const;

export const settingsSections = [
  { key: 'config', labelKey: 'settings.general' },
  { key: 'automation', labelKey: 'settings.automation' },
  { key: 'registrar', labelKey: 'settings.registrar' },
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
