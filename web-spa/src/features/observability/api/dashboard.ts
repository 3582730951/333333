import { z } from 'zod';
import { get } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import { systemMetricsSchema } from './system';
import { usageDashboardSchema } from './usage';
import type {
  AccountPoolSummary, DashboardCore, DashboardHealth, DashboardSecondary, RegistrationStats,
} from '../model/dashboard';
import type { SystemMetrics } from '../model/system';
import type { UsageBucket, UsageCacheReport, UsageMetricRow, UsageSeriesDescriptor } from '../model/usage';

export const DASHBOARD_REFRESH_MS = 15_000;

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

interface DashboardUsageSnapshot {
  buckets: UsageBucket[];
  modelSeries: UsageMetricRow[];
  series: UsageSeriesDescriptor[];
  byModel: UsageMetricRow[];
  cache: UsageCacheReport;
  timeseriesAvailable: boolean;
  modelAvailable: boolean;
  cacheAvailable: boolean;
}

interface DashboardUsageFlight {
  controller: AbortController;
  consumers: Set<symbol>;
  promise: Promise<DashboardUsageSnapshot>;
  settled: boolean;
  settledAt: number;
}

// Core and secondary retain separate query keys, so their polling timers can
// drift. Keep the shared snapshot for one refresh interval while allowing an
// explicit manual-refresh invalidation.
let dashboardUsageFlight: DashboardUsageFlight | null = null;

async function requestDashboardUsage(signal: AbortSignal): Promise<DashboardUsageSnapshot> {
  const now = Math.floor(Date.now() / 1000);
  const parsed = parseApiResponse(usageDashboardSchema, await get('/admin/usage/dashboard', {
    timeseries_since: now - 86400,
    models_since: now - 7 * 86400,
    bucket: 3600,
    dimension: 'provider_model',
    series_dimension: 'provider_model',
    series_limit: 8,
    fields: 'summary,by_account,by_provider,by_provider_model',
    allow_partial: true,
  }, { signal }));
  const unavailable = new Set(parsed.unavailable_sections ?? []);
  return {
    buckets: (parsed.timeseries ?? []) as UsageBucket[],
    modelSeries: (parsed.model_series ?? []) as UsageMetricRow[],
    series: (parsed.series ?? []) as UsageSeriesDescriptor[],
    byModel: (parsed.models ?? []) as UsageMetricRow[],
    cache: (parsed.cache ?? {}) as UsageCacheReport,
    timeseriesAvailable: !unavailable.has('timeseries'),
    modelAvailable: !unavailable.has('models'),
    cacheAvailable: !unavailable.has('cache'),
  };
}

function abortReason(signal?: AbortSignal) {
  return signal?.reason ?? new DOMException('The operation was aborted.', 'AbortError');
}

function fetchDashboardUsage(signal?: AbortSignal): Promise<DashboardUsageSnapshot> {
  if (signal?.aborted) return Promise.reject(abortReason(signal));
  let flight = dashboardUsageFlight;
  if (flight?.settled && Date.now() - flight.settledAt >= DASHBOARD_REFRESH_MS) {
    dashboardUsageFlight = null;
    flight = null;
  }
  if (!flight) {
    const controller = new AbortController();
    let created!: DashboardUsageFlight;
    created = {
      controller,
      consumers: new Set(),
      settled: false,
      settledAt: 0,
      promise: requestDashboardUsage(controller.signal).finally(() => {
        created.settled = true;
        created.settledAt = Date.now();
      }),
    };
    dashboardUsageFlight = created;
    flight = created;
  }

  const consumer = Symbol('dashboard-usage-consumer');
  flight.consumers.add(consumer);
  let completed = false;
  const result = new Promise<DashboardUsageSnapshot>((resolve, reject) => {
    let onAbort: () => void;
    const finish = () => {
      if (completed) return;
      completed = true;
      signal?.removeEventListener('abort', onAbort);
    };
    const succeed = (value: DashboardUsageSnapshot) => {
      if (completed) return;
      finish();
      resolve(value);
    };
    const fail = (error: unknown) => {
      if (completed) return;
      finish();
      reject(error);
    };
    onAbort = () => fail(abortReason(signal));
    signal?.addEventListener('abort', onAbort, { once: true });
    if (signal?.aborted) onAbort();
    flight.promise.then(succeed, fail);
  });
  return result.finally(() => {
    flight.consumers.delete(consumer);
    if (!flight.settled && flight.consumers.size === 0) {
      if (dashboardUsageFlight === flight) dashboardUsageFlight = null;
      flight.controller.abort();
    }
  });
}

export function invalidateDashboardUsageSnapshot() {
  const flight = dashboardUsageFlight;
  dashboardUsageFlight = null;
  if (flight && !flight.settled && flight.consumers.size === 0) flight.controller.abort();
}

async function fetchRegistrationStats(signal?: AbortSignal): Promise<RegistrationStats> {
  return parseApiResponse(registrationStatsSchema, await get('/admin/register/stats', undefined, { signal }));
}

async function fetchDashboardSystem(signal?: AbortSignal): Promise<SystemMetrics> {
  return parseApiResponse(systemMetricsSchema, await get('/admin/system', undefined, { signal })) as SystemMetrics;
}

export async function fetchDashboardCore(signal?: AbortSignal): Promise<DashboardCore> {
  const results = await Promise.allSettled([
    fetchHealth(signal), fetchAccountSummary(signal),
  ]);
  if (results[1].status === 'rejected') throw results[1].reason;
  const failures = results[0].status === 'rejected' ? [results[0].reason] : [];
  return {
    health: results[0].status === 'fulfilled' ? results[0].value : null,
    accountSummary: results[1].value,
    // Usage aggregation is intentionally secondary. Account state now paints as
    // soon as its small summary query finishes instead of waiting for telemetry
    // flush and several aggregate scans.
    buckets: [],
    modelSeries: [],
    series: [],
    healthAvailable: results[0].status === 'fulfilled',
    timeseriesAvailable: false,
    error: failures.length ? partialError('DASHBOARD_CORE_PARTIAL', '部分核心指标暂时不可用。', failures) : null,
  };
}

export async function fetchDashboardSecondary(signal?: AbortSignal): Promise<DashboardSecondary> {
  const results = await Promise.allSettled([
    fetchRegistrationStats(signal), fetchDashboardSystem(signal), fetchDashboardUsage(signal),
  ]);
  // Registration and host metrics are optional dashboard enhancements. A deployment
  // without the registration subsystem, or a transient /admin/system failure, should
  // hide only that card rather than presenting a page-wide red diagnostic alarm.
  // The aggregate usage snapshot supplies model and cache diagnostics together; its
  // failure surfaces while any successful registration/system result remains usable.
  const usage = results[2].status === 'fulfilled' ? results[2].value : null;
  const timeseriesAvailable = Boolean(usage?.timeseriesAvailable);
  const modelAvailable = Boolean(usage?.modelAvailable);
  const cacheAvailable = Boolean(usage?.cacheAvailable);
  const diagnosticFailures = results[2].status === 'rejected'
    ? [results[2].reason]
    : [
        ...(!modelAvailable ? [new Error('dashboard models unavailable')] : []),
        ...(!cacheAvailable ? [new Error('dashboard cache unavailable')] : []),
      ];
  const usageDiagnosticsUnavailable = !modelAvailable && !cacheAvailable;
  return {
    registration: results[0].status === 'fulfilled' ? results[0].value : null,
    system: results[1].status === 'fulfilled' ? results[1].value : null,
    buckets: timeseriesAvailable && usage ? usage.buckets : [],
    modelSeries: timeseriesAvailable && usage ? usage.modelSeries : [],
    series: timeseriesAvailable && usage ? usage.series : [],
    byModel: modelAvailable && usage ? usage.byModel : [],
    cache: cacheAvailable && usage ? usage.cache : null,
    registrationAvailable: results[0].status === 'fulfilled',
    systemAvailable: results[1].status === 'fulfilled',
    timeseriesAvailable,
    modelAvailable,
    cacheAvailable,
    error: usageDiagnosticsUnavailable ? partialError('DASHBOARD_SECONDARY_UNAVAILABLE', '用量诊断暂时不可用。', diagnosticFailures) : null,
  };
}
