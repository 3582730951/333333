import { QueryClient } from '@tanstack/react-query';
import { describe, expect, it } from 'vitest';
import { z } from 'zod';
import { pageResultSchema, parseApiResponse } from '../src/api/contracts';
import { normalizeApiError } from '../src/api/errors';
import { adminRoutes, adminVisualRoutes, legacyRedirects, portalRoutes, settingsSections } from '../src/app/routeDefinitions';
import { allowedResponsiveActions } from '../src/components/ResponsiveDataView';
import { accountImportSchema, apiKeyFormSchema, userFormSchema } from '../src/features/access/model/forms';
import { cleanApiKeyValues } from '../src/components/ApiKeyCreateModal';
import { filenameFromDisposition } from '../src/features/observability/api/exports';
import { keysResponseSchema } from '../src/features/access/api/keys';
import { usersResponseSchema } from '../src/features/access/api/users';
import { accountsResponseSchema } from '../src/features/accounts/api/accounts';
import { cfEventsResponseSchema, quotaResponseSchema } from '../src/features/observability/api/events';
import {
  lifecycleProviderOptionsSchema, lifecycleServicesResponseSchema, lifecycleTasksResponseSchema,
} from '../src/features/automation/api/lifecycle';
import { LIFECYCLE_REFETCH_INTERVAL, lifecycleQueryKeys } from '../src/features/automation/queries/lifecycle';
import {
  adaptRegistrationStrategy, registrationCountriesSchema, registrationJobsResponseSchema,
  registrationReadinessSchema,
} from '../src/features/automation/api/registration';
import {
  lockedIdentityForMethod, manualStartBlockers, methodUsesSMSCountry,
} from '../src/features/automation/model/registration';
import { REGISTRATION_REFETCH_INTERVAL, registrationQueryKeys } from '../src/features/automation/queries/registration';
import { systemMetricsSchema } from '../src/features/observability/api/system';
import { SYSTEM_REFETCH_INTERVAL, systemQueryKeys } from '../src/features/observability/queries/system';
import {
  FULL_CACHE_FIELDS, usageCacheSchema, usageEnvelopeSchema, usageMetricRowSchema, usageTimeseriesSchema,
} from '../src/features/observability/api/usage';
import { usageQueryKeys } from '../src/features/observability/queries/usage';
import {
  automationSectionSchema, configFieldsResponseSchema, lifecycleSectionSchema, providersResponseSchema,
  moderationSettingsSchema, settingsProviderOptionsSchema, settingsSaveResponseSchema, settingsTemplateSchema,
  thinkingSettingsSchema,
} from '../src/features/settings/api/settings';
import { settingsQueryKeys } from '../src/features/settings/queries/settings';
import { accountPoolSummarySchema, dashboardHealthSchema, registrationStatsSchema } from '../src/features/observability/api/dashboard';
import { DASHBOARD_REFRESH_MS, dashboardQueryKeys } from '../src/features/observability/queries/dashboard';
import { portalUsageResponseSchema } from '../src/features/portal/api/usage';
import { portalUsageQueryKeys } from '../src/features/portal/queries/usage';
import { invalidateQueryKeys, queryKeys } from '../src/features/shared/queries';
import { responsiveState } from '../src/lib/breakpoints';
import type { ResponsiveDataView } from '../src/model/contracts';

describe('API contracts', () => {
  const schema = pageResultSchema(z.object({ id: z.string() }));

  it('normalizes array and paged response shapes', () => {
    expect(parseApiResponse(schema, [{ id: 'a' }])).toEqual({ rows: [{ id: 'a' }], total: 1, page: 1, pageSize: 1 });
    expect(parseApiResponse(schema, { items: [{ id: 'b' }], total: 9, page: 2, page_size: 4 })).toEqual({
      rows: [{ id: 'b' }], total: 9, page: 2, pageSize: 4,
    });
  });

  it('rejects malformed responses with a request id', () => {
    expect(() => parseApiResponse(schema, { rows: [{ id: 1 }] }, 'req-42')).toThrowError(expect.objectContaining({
      code: 'INVALID_RESPONSE', requestId: 'req-42', retryable: false,
    }));
  });

  it('normalizes transport errors without leaking response structure', () => {
    const error = normalizeApiError({
      isAxiosError: true,
      code: 'ERR_BAD_RESPONSE',
      response: {
        status: 503,
        data: { error: { code: 'UPSTREAM_BUSY', message: '请稍后重试', request_id: 'req-99' } },
        headers: {},
      },
    });
    expect(error).toMatchObject({ status: 503, code: 'UPSTREAM_BUSY', requestId: 'req-99', retryable: true, userMessage: '请稍后重试' });
  });

  it('adapts legacy domain response envelopes without dropping unknown fields', () => {
    expect(parseApiResponse(accountsResponseSchema, {
      accounts: [{ id: 'acc-1', provider: 'codex', future_flag: true }], total: '4',
    })).toEqual({ rows: [{ id: 'acc-1', provider: 'codex', future_flag: true }], total: 4 });
    expect(parseApiResponse(quotaResponseSchema, { quota: [{ account_id: 'acc-1', future_limit: 12 }] }))
      .toEqual([{ account_id: 'acc-1', future_limit: 12 }]);
    expect(parseApiResponse(cfEventsResponseSchema, { events: [{ id: 7, status: '403' }] }))
      .toEqual([{ id: 7, status: '403' }]);
    expect(parseApiResponse(usersResponseSchema, {
      rows: [{ id: 'user-1', email: 'user@example.com', role: 'user', status: 'active' }],
    })).toHaveLength(1);
    expect(parseApiResponse(keysResponseSchema, { keys: [{ hash: 'hash-1', enabled: true }] }))
      .toEqual([{ hash: 'hash-1', enabled: true }]);
  });

  it('surfaces malformed domain rows as contract errors', () => {
    expect(() => parseApiResponse(quotaResponseSchema, { rows: [{ account_id: 123 }] }))
      .toThrowError(expect.objectContaining({ code: 'INVALID_RESPONSE' }));
    expect(() => parseApiResponse(usersResponseSchema, [{ id: 'user-1', role: 'user' }]))
      .toThrowError(expect.objectContaining({ code: 'INVALID_RESPONSE' }));
  });

  it('normalizes lifecycle envelopes and service maps', () => {
    expect(parseApiResponse(lifecycleTasksResponseSchema, { tasks: [{ id: 'job-1', status: 'pending', future_field: 1 }] }))
      .toEqual([{ id: 'job-1', status: 'pending', future_field: 1 }]);
    expect(parseApiResponse(lifecycleServicesResponseSchema, { services: [] })).toEqual([]);
    expect(parseApiResponse(lifecycleServicesResponseSchema, {
      sms: { name: 'SMS', status: 'alive' }, mailbox: 'unreachable',
    })).toEqual([{ name: 'SMS', status: 'alive' }, { name: 'unreachable', status: 'unreachable' }]);
    expect(parseApiResponse(lifecycleProviderOptionsSchema, {
      sms: ['sms-a'], mailbox: [{ label: 'Mailbox A', value: 'mail-a', future: true }],
    })).toEqual({ sms: ['sms-a'], mailbox: [{ label: 'Mailbox A', value: 'mail-a', future: true }], captcha: [] });
  });

  it('adapts registration responses and method-specific blockers', () => {
    expect(parseApiResponse(registrationJobsResponseSchema, { jobs: [{ id: 'register-1', status: 'running', future: true }] }))
      .toEqual([{ id: 'register-1', status: 'running', future: true }]);
    const readiness = parseApiResponse(registrationReadinessSchema, {
      ready: false,
      providers: { sms: '1', mailbox: 0, email_otp: 0 },
      blockers: ['auto-refill 策略未启用'],
    });
    expect(manualStartBlockers(readiness, 'phone', 'node')).toEqual([]);
    expect(manualStartBlockers(readiness, 'email', 'protocol')).toEqual([{ code: 'mailbox_missing' }]);
    expect(manualStartBlockers(readiness, 'email', 'protocol_v2')).toEqual([{ code: 'email_otp_missing' }]);
    expect(lockedIdentityForMethod('browser_v3')).toBe('email');
    expect(methodUsesSMSCountry('browser_v3', 'email')).toBe(true);
    expect(parseApiResponse(registrationCountriesSchema, [{ isoCode: 'US', name: 'United States' }]))
      .toEqual([{ isoCode: 'US', name: 'United States', nameZh: '' }]);
    expect(adaptRegistrationStrategy([
      { key: 'sms_platform_strategy', value: 'manual' },
      { key: 'sms_manual_country', value: 'US' },
      { key: 'default_register_method', value: 'BROWSER_V3' },
    ])).toEqual({ strategy: 'manual', manualCountry: 'US', defaultMethod: 'browser_v3' });
  });

  it('normalizes system metrics while preserving supervisor diagnostics', () => {
    expect(parseApiResponse(systemMetricsSchema, {
      supported: true,
      cpu: { usage_pct: '12.5', cores: '4', future: true },
      registration: { procs: [{ pid: 42, kind: 'node', rss_kb: '1024' }] },
      supervisor_modules: [{ name: 'worker', status: 'failed', unexpected_exit_count: '2', last_message: 'boom' }],
      supervisor_events: [{ module: 'worker', type: 'unexpected_exit', time_unix: '1700000000' }],
    })).toMatchObject({
      supported: true,
      cpu: { usage_pct: 12.5, cores: 4, future: true },
      registration: { procs: [{ pid: 42, kind: 'node', rss_kb: 1024 }] },
      supervisor_modules: [{ name: 'worker', status: 'failed', unexpected_exit_count: 2, last_message: 'boom' }],
      supervisor_events: [{ module: 'worker', type: 'unexpected_exit', time_unix: 1_700_000_000 }],
    });
    expect(() => parseApiResponse(systemMetricsSchema, { supported: true, supervisor_modules: 'broken' }))
      .toThrowError(expect.objectContaining({ code: 'INVALID_RESPONSE' }));
  });

  it('normalizes usage envelopes, metrics, and model timeseries', () => {
    expect(parseApiResponse(usageEnvelopeSchema, {
      usage: [{ account_id: 'acc-1', requests: '4', total_tokens: '1200', future_metric: true }],
      effective_start_at: '1700000000',
    })).toMatchObject({
      rows: [{ account_id: 'acc-1', requests: 4, total_tokens: 1200, future_metric: true }],
      effective_start_at: 1_700_000_000,
    });
    expect(parseApiResponse(usageMetricRowSchema, {
      model: 'gpt-5', cache_read_share: '0.25', latest_user_cache_control: 3,
    })).toMatchObject({ model: 'gpt-5', cache_read_share: 0.25, latest_user_cache_control: 3 });
    expect(parseApiResponse(usageTimeseriesSchema, {
      buckets: [{ bucket: '1700000000', total_tokens: '10' }],
      model_series: [{ bucket: '1700000000', series_key: 'gpt-5', total_tokens: '8' }],
      series: [{ series_dimension: 'model', series_key: 'gpt-5', series_label: 'GPT-5' }],
    })).toEqual({
      buckets: [{ bucket: 1_700_000_000, total_tokens: 10 }],
      modelSeries: [{ bucket: 1_700_000_000, series_key: 'gpt-5', total_tokens: 8 }],
      series: [{ series_dimension: 'model', series_key: 'gpt-5', series_label: 'GPT-5' }],
    });
    expect(() => parseApiResponse(usageCacheSchema, { by_route: 'broken' }))
      .toThrowError(expect.objectContaining({ code: 'INVALID_RESPONSE' }));
    expect(parseApiResponse(usageCacheSchema, {
      summary: { latest_user_cache_control: 2 },
      by_account: null,
      by_model: null,
      by_api_key: null,
      by_account_model: null,
      by_route: null,
      by_route_account_model: null,
      by_time_bucket: null,
    })).toMatchObject({ summary: { latest_user_cache_control: 2 } });
    expect(parseApiResponse(usageTimeseriesSchema, { buckets: null, model_series: null, series: null }))
      .toEqual({ buckets: [], modelSeries: [], series: [] });
  });

  it('normalizes settings sections and preserves unknown configuration fields', () => {
    expect(parseApiResponse(configFieldsResponseSchema, {
      fields: [
        { key: 'require_downstream_key', type: 'bool', value: true },
        { key: 'registration_concurrency', type: 'int', value: 3 },
        { key: 'model_quality_models', type: 'csv', value: 'gpt-5,gpt-5-mini' },
      ],
    })).toMatchObject([
      { key: 'require_downstream_key', type: 'bool', value: true, category: '运行时配置' },
      { key: 'registration_concurrency', type: 'int', value: 3, category: '运行时配置' },
      { key: 'model_quality_models', type: 'csv', value: 'gpt-5,gpt-5-mini', category: '运行时配置' },
    ]);
    expect(parseApiResponse(automationSectionSchema, { automation: {
      policies: [{ type: 'refill', enabled: true, config: { target: 10 }, future: 1 }],
      stats: { running: 2 }, readiness: { ready: false, blockers: ['provider missing'] }, policy_error: 'legacy row',
    } })).toMatchObject({
      policies: { refill: { type: 'refill', enabled: true, config: { target: 10 }, future: 1 } },
      stats: { running: 2 }, readiness: { ready: false, blockers: ['provider missing'] },
      automationErrors: { policy: 'legacy row', stats: '' },
    });
    expect(parseApiResponse(lifecycleSectionSchema, { lifecycle: { defaults: { sms: 'hero' }, defaults_error: '' } }))
      .toEqual({ defaults: { sms: 'hero' }, defaultsError: '' });
    expect(parseApiResponse(providersResponseSchema, { providers: [{ type: 'sms', key: 'hero', priority: '90' }] }))
      .toEqual([{ type: 'sms', key: 'hero', priority: 90 }]);
    expect(parseApiResponse(settingsProviderOptionsSchema, { sms: ['hero'] })).toMatchObject({ sms: ['hero'], mailbox: [], captcha: [] });
  });

  it('validates settings mutation and template responses', () => {
    expect(parseApiResponse(settingsSaveResponseSchema, {})).toEqual({ saved: [] });
    expect(parseApiResponse(settingsTemplateSchema, { id: 'email-only', name: 'Email only', needs: ['mailProvider'] }))
      .toMatchObject({ id: 'email-only', name: 'Email only', needs: ['mailProvider'], saved: [] });
    expect(() => parseApiResponse(automationSectionSchema, { automation: { policies: [{ enabled: true }] } }))
      .toThrowError(expect.objectContaining({ code: 'INVALID_RESPONSE' }));
  });

  it('validates thinking and moderation settings without dropping future fields', () => {
    expect(parseApiResponse(thinkingSettingsSchema, {
      enabled: true, default_mode: 'level', default_level: 'high', default_budget: '4096',
      providers: { anthropic: { mode: 'budget', budget: '8192' } }, models: {}, future_setting: true,
    })).toMatchObject({
      enabled: true, default_budget: 4096,
      providers: { anthropic: { mode: 'budget', budget: 8192 } }, future_setting: true,
    });
    expect(parseApiResponse(moderationSettingsSchema, {
      enabled: false, model: 'gpt-5-mini', auto_translate: true, words: ['secret'], future_setting: 1,
    })).toMatchObject({ enabled: false, model: 'gpt-5-mini', auto_translate: true, words: ['secret'], future_setting: 1 });
    expect(() => parseApiResponse(thinkingSettingsSchema, {
      enabled: true, default_mode: 'level', default_level: 'high', default_budget: -1, providers: {}, models: {},
    })).toThrowError(expect.objectContaining({ code: 'INVALID_RESPONSE' }));
    expect(() => parseApiResponse(moderationSettingsSchema, {
      enabled: false, model: '', auto_translate: false, words: 'secret',
    })).toThrowError(expect.objectContaining({ code: 'INVALID_RESPONSE' }));
  });

  it('normalizes dashboard summaries, registration trends, and portal usage', () => {
    expect(parseApiResponse(accountPoolSummarySchema, {
      total: '4', active: '2', quarantined: 1, cooling: 1, recheck: 0, codex: 3, claude: 1,
    })).toEqual({ total: 4, active: 2, quarantined: 1, cooling: 1, recheck: 0, codex: 3, claude: 1, other: 0 });
    expect(parseApiResponse(dashboardHealthSchema, { ok: true, version: 'future' })).toEqual({ ok: true, version: 'future' });
    expect(parseApiResponse(registrationStatsSchema, {
      totals: { success_rate: '0.75', succeeded: '3', failed: 1 },
      by_day: [{ date: '2026-07-11', succeeded: '2', failed: '1' }],
    })).toMatchObject({ totals: { success_rate: 0.75, succeeded: 3, failed: 1 }, by_day: [{ succeeded: 2, failed: 1 }] });
    expect(parseApiResponse(portalUsageResponseSchema, {
      usage: [{ model: 'gpt-5', requests: '2', prompt_tokens: '100', completion_tokens: '20', total_tokens: '120', future: true }],
    })).toEqual([{ model: 'gpt-5', requests: 2, prompt_tokens: 100, completion_tokens: 20, total_tokens: 120, future: true }]);
    expect(() => parseApiResponse(accountPoolSummarySchema, { total: -1 }))
      .toThrowError(expect.objectContaining({ code: 'INVALID_RESPONSE' }));
    expect(() => parseApiResponse(portalUsageResponseSchema, [{ requests: 1 }]))
      .toThrowError(expect.objectContaining({ code: 'INVALID_RESPONSE' }));
  });
});

describe('routing, responsive actions, and forms', () => {
  it('keeps every management and portal screen in the visual route matrix', () => {
    expect(adminVisualRoutes).toHaveLength(21);
    expect(portalRoutes).toHaveLength(4);
    expect(new Set(adminRoutes.map((route) => route.path)).size).toBe(adminRoutes.length);
    expect(settingsSections.map((section) => section.key)).toEqual(['config', 'automation', 'registrar', 'lifecycle', 'logging', 'memory', 'thinking', 'moderation']);
    expect(legacyRedirects.find((route) => route.path === '/thinking')?.to).toContain('?tab=thinking');
  });

  it('uses the same 768px mobile boundary for shell and data actions', () => {
    expect(responsiveState(767).isMobile).toBe(true);
    expect(responsiveState(768).isMobile).toBe(false);
    const definition: ResponsiveDataView<{ id: string }> = {
      desktopColumns: [], mobileSummary: () => null, details: () => null,
      actions: [
        { key: 'copy', label: '复制', mobile: 'allow', run: () => undefined },
        { key: 'delete', label: '删除', mobile: 'desktop-only', run: () => undefined },
      ],
    };
    expect(allowedResponsiveActions(definition, true).map((action) => action.key)).toEqual(['copy']);
    expect(allowedResponsiveActions(definition, false)).toHaveLength(2);
  });

  it('validates API key and account import forms', () => {
    expect(apiKeyFormSchema.safeParse({ label: '', group_name: '', force_model: '', force_effort: '', key_type: 'downstream', expires_at: '' }).success).toBe(false);
    expect(apiKeyFormSchema.safeParse({ label: 'CLI', group_name: '', force_model: '', force_effort: 'ultra', key_type: 'downstream', expires_at: '2026-12-31T23:59:59Z' }).success).toBe(true);
    expect(accountImportSchema.safeParse({ provider: 'codex', credential: 'secret', label: '', group_name: '' }).success).toBe(true);
    expect(userFormSchema.safeParse({ email: 'invalid', name: '', role: 'user', status: 'active', password: '' }).success).toBe(false);
    expect(userFormSchema.safeParse({ email: 'user@example.com', name: '', role: 'user', status: 'active', password: 'password' }).success).toBe(true);
  });

  it('normalizes key forms and audit archive filenames', () => {
    expect(cleanApiKeyValues({
      label: ' CLI ', key_type: 'pool_import', group_name: ' team ', force_model: ' gpt-5 ',
      force_effort: 'high', expires_at: '2027-01-01T00:00:00Z',
    }, 'admin')).toEqual({
      label: 'CLI', key_type: 'pool_import', group_name: 'team', force_model: 'gpt-5',
      force_effort: 'high', expires_at: 1798761600,
    });
    expect(() => cleanApiKeyValues({ label: 'CLI', expires_at: 'tomorrow-ish' }, 'admin')).toThrow('过期时间格式无效');
    expect(filenameFromDisposition("attachment; filename*=UTF-8''diagnostics%20bundle.zip")).toBe('diagnostics bundle.zip');
    expect(filenameFromDisposition('attachment; filename="cache-hits.zip"')).toBe('cache-hits.zip');
  });
});

describe('query invalidation', () => {
  it('invalidates domain keys after a mutation', async () => {
    const client = new QueryClient();
    const key = queryKeys.list('accounts');
    client.setQueryData(key, [{ id: 'a' }]);
    await invalidateQueryKeys(client, [queryKeys.domain('accounts')]);
    expect(client.getQueryState(key)?.isInvalidated).toBe(true);
  });

  it('keeps lifecycle polling visible-page scoped and domain keyed', () => {
    expect(LIFECYCLE_REFETCH_INTERVAL).toBe(5_000);
    expect(lifecycleQueryKeys.dashboard.slice(0, 2)).toEqual(['pool', 'lifecycle-dashboard']);
  });

  it('uses separate registration keys for polling and reference data', () => {
    expect(REGISTRATION_REFETCH_INTERVAL).toBe(5_000);
    expect(registrationQueryKeys.dashboard).not.toEqual(registrationQueryKeys.countries);
    expect(registrationQueryKeys.strategy).not.toEqual(registrationQueryKeys.options);
  });

  it('polls system metrics independently every three seconds', () => {
    expect(SYSTEM_REFETCH_INTERVAL).toBe(3_000);
    expect(systemQueryKeys.metrics.slice(0, 2)).toEqual(['pool', 'system-metrics']);
  });

  it('keys usage dashboards by range and requests every cache diagnostic dimension', () => {
    expect(usageQueryKeys.dashboard('today')).not.toEqual(usageQueryKeys.dashboard(604800));
    expect(usageQueryKeys.dashboard(604800)).not.toEqual(usageQueryKeys.dashboard(2592000));
    expect(FULL_CACHE_FIELDS.split(',')).toEqual([
      'summary', 'by_account', 'by_model', 'by_api_key', 'by_account_model',
      'by_provider', 'by_provider_model',
      'by_route', 'by_route_account_model', 'by_time_bucket',
    ]);
  });

  it('keeps each settings section and shared option query independently addressable', () => {
    expect(settingsQueryKeys.section('config')).not.toEqual(settingsQueryKeys.section('memory'));
    expect(settingsQueryKeys.sharedOptions).not.toEqual(settingsQueryKeys.section('lifecycle'));
    expect(settingsQueryKeys.section('config').slice(0, 2)).toEqual(['pool', 'settings']);
    expect(settingsQueryKeys.advanced('thinking')).not.toEqual(settingsQueryKeys.advanced('moderation'));
  });

  it('polls dashboard layers independently and keeps portal usage separately keyed', () => {
    expect(DASHBOARD_REFRESH_MS).toBe(15_000);
    expect(dashboardQueryKeys.core).not.toEqual(dashboardQueryKeys.secondary);
    expect(dashboardQueryKeys.core.slice(0, 2)).toEqual(['pool', 'dashboard']);
    expect(portalUsageQueryKeys.dashboard.slice(0, 2)).toEqual(['pool', 'portal-usage']);
  });
});
