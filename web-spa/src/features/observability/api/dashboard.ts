import { z } from 'zod';
import { get } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import { systemMetricsSchema } from './system';
import { usageByModelSchema, usageCacheSchema, usageTimeseriesSchema } from './usage';
import type {
  AccountPoolSummary, DashboardCore, DashboardHealth, DashboardSecondary, RegistrationStats,
} from '../model/dashboard';
import type { SystemMetrics } from '../model/system';
import type { UsageBucket, UsageCacheReport, UsageMetricRow, UsageSeriesDescriptor } from '../model/usage';

export const dashboardHealthSchema = z.object({ ok: z.boolean() }).passthrough();
export const accountPoolSummarySchema = z.object({
  total: z.coerce.number().int().nonnegative(),
  active: z.coerce.number().int().nonnegative(),
  quarantined: z.coerce.number().int().nonnegative(),
  cooling: z.coerce.number().int().nonnegative(),
  recheck: z.coerce.number().int().nonnegative(),
  codex: z.coerce.number().int().nonnegative(),
  claude: z.coerce.number().int().nonnegative(),
  other: z.coerce.number().int().nonnegative().optional(),
}).passthrough().transform((value) => ({ ...value, other: value.other ?? 0 }));

const registrationDaySchema = z.object({
  date: z.string().optional(),
  succeeded: z.coerce.number().nonnegative().optional(),
  failed: z.coerce.number().nonnegative().optional(),
}).passthrough();
export const registrationStatsSchema = z.object({
  totals: z.object({
    success_rate: z.coerce.number().nonnegative().optional(),
    succeeded: z.coerce.number().nonnegative().optional(),
    failed: z.coerce.number().nonnegative().optional(),
  }).passthrough().optional(),
  by_day: z.array(registrationDaySchema).optional(),
}).passthrough();

function partialError(code: string, message: string, failures: unknown[]) {
  return createApiError({ code, userMessage: message, retryable: true, cause: failures });
}

async function fetchHealth(signal?: AbortSignal): Promise<DashboardHealth> {
  return parseApiResponse(dashboardHealthSchema, await get('/healthz', undefined, { signal }));
}

async function fetchAccountSummary(signal?: AbortSignal): Promise<AccountPoolSummary> {
  return parseApiResponse(accountPoolSummarySchema, await get('/admin/accounts/summary', undefined, { signal }));
}

async function fetchDashboardTimeseries(signal?: AbortSignal): Promise<{ buckets: UsageBucket[]; modelSeries: UsageMetricRow[]; series: UsageSeriesDescriptor[] }> {
  const now = Math.floor(Date.now() / 1000);
  return parseApiResponse(usageTimeseriesSchema, await get('/admin/usage/timeseries', {
    since: now - 86400,
    bucket: 3600,
    series_dimension: 'provider_model',
    series_limit: 8,
  }, { signal }));
}

async function fetchRegistrationStats(signal?: AbortSignal): Promise<RegistrationStats> {
  return parseApiResponse(registrationStatsSchema, await get('/admin/register/stats', undefined, { signal }));
}

async function fetchDashboardSystem(signal?: AbortSignal): Promise<SystemMetrics> {
  return parseApiResponse(systemMetricsSchema, await get('/admin/system', undefined, { signal })) as SystemMetrics;
}

async function fetchDashboardModels(signal?: AbortSignal): Promise<UsageMetricRow[]> {
  const now = Math.floor(Date.now() / 1000);
  return parseApiResponse(usageByModelSchema, await get('/admin/usage/by-model', { since: now - 7 * 86400, dimension: 'provider_model' }, { signal })) as UsageMetricRow[];
}

async function fetchDashboardCache(signal?: AbortSignal): Promise<UsageCacheReport> {
  return parseApiResponse(usageCacheSchema, await get('/admin/usage/cache', { fields: 'summary,by_account,by_provider,by_provider_model' }, { signal })) as UsageCacheReport;
}

export async function fetchDashboardCore(signal?: AbortSignal): Promise<DashboardCore> {
  const results = await Promise.allSettled([
    fetchHealth(signal), fetchAccountSummary(signal), fetchDashboardTimeseries(signal),
  ]);
  if (results[1].status === 'rejected') throw results[1].reason;
  const failures = [results[0], results[2]].filter((result) => result.status === 'rejected').map((result) => result.reason);
  return {
    health: results[0].status === 'fulfilled' ? results[0].value : null,
    accountSummary: results[1].value,
    buckets: results[2].status === 'fulfilled' ? results[2].value.buckets : [],
    modelSeries: results[2].status === 'fulfilled' ? results[2].value.modelSeries : [],
    series: results[2].status === 'fulfilled' ? results[2].value.series : [],
    healthAvailable: results[0].status === 'fulfilled',
    timeseriesAvailable: results[2].status === 'fulfilled',
    error: failures.length ? partialError('DASHBOARD_CORE_PARTIAL', '部分核心指标暂时不可用。', failures) : null,
  };
}

export async function fetchDashboardSecondary(signal?: AbortSignal): Promise<DashboardSecondary> {
  const results = await Promise.allSettled([
    fetchRegistrationStats(signal), fetchDashboardSystem(signal), fetchDashboardModels(signal), fetchDashboardCache(signal),
  ]);
  // Registration and host metrics are optional dashboard enhancements. A deployment
  // without the registration subsystem, or a transient /admin/system failure, should
  // hide only that card rather than presenting a page-wide red diagnostic alarm.
  // Model/cache failures surface only when both sources are unavailable. If either
  // source still has useful data, its card remains visible without a duplicate global
  // alarm—the unavailable card is already omitted by its availability flag.
  const diagnosticFailures = results.slice(2).filter((result) => result.status === 'rejected').map((result) => result.reason);
  const usageDiagnosticsUnavailable = results[2].status === 'rejected' && results[3].status === 'rejected';
  return {
    registration: results[0].status === 'fulfilled' ? results[0].value : null,
    system: results[1].status === 'fulfilled' ? results[1].value : null,
    byModel: results[2].status === 'fulfilled' ? results[2].value : [],
    cache: results[3].status === 'fulfilled' ? results[3].value : null,
    registrationAvailable: results[0].status === 'fulfilled',
    systemAvailable: results[1].status === 'fulfilled',
    modelAvailable: results[2].status === 'fulfilled',
    cacheAvailable: results[3].status === 'fulfilled',
    error: usageDiagnosticsUnavailable ? partialError('DASHBOARD_SECONDARY_UNAVAILABLE', '用量诊断暂时不可用。', diagnosticFailures) : null,
  };
}
