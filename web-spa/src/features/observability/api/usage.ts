import { z } from 'zod';
import { get, post } from '../../../api.js';
import { parseApiResponse } from '../../../api/contracts';
import type {
  UsageBucket, UsageCacheReport, UsageDashboard, UsageEnvelope, UsageMetricRow,
  UsageRange, UsageSeriesDescriptor,
} from '../model/usage';

export const FULL_CACHE_FIELDS = 'summary,by_account,by_model,by_api_key,by_account_model,by_provider,by_provider_model,by_route,by_route_account_model,by_time_bucket';

const numericKeys = [
  'requests', 'real_requests', 'hit_requests', 'request_hit_rate', 'token_hit_rate', 'real_token_hit_rate',
  'eligible_cache_hit_rate', 'cache_write_share', 'cache_read_share', 'prompt_tokens', 'completion_tokens', 'total_tokens',
  'cached_tokens', 'cache_input_tokens', 'cache_read_tokens', 'cache_creation_tokens', 'cache_miss_tokens',
  'cache_creation_5m_share', 'stable_prefix_bytes', 'cache_breakpoint_count', 'estimated_rate', 'bucket',
  'latest_user_cache_control',
  'latest_user_auto_context_cache_control', 'latest_user_tail_cache_control', 'latest_user_tool_result_cache_control',
  'actual_requests', 'actual_prompt_tokens', 'actual_completion_tokens', 'actual_total_tokens',
  'estimated_requests', 'estimated_prompt_tokens', 'estimated_completion_tokens', 'estimated_total_tokens',
  'combined_requests', 'combined_total_tokens', 'kiro_credits', 'kiro_credits_reported_requests',
  'cache_reported_requests', 'cache_unreported_requests', 'cache_reporting_rate', 'usage_complete_through_at',
  'pending_usage_requests', 'usage_lag_seconds',
] as const;

const numericShape = Object.fromEntries(numericKeys.map((key) => [key, z.coerce.number().optional()]));
export const usageMetricRowSchema = z.object({
  ...numericShape,
  account_id: z.string().optional(),
  label: z.string().optional(),
  model: z.string().optional(),
  model_key: z.string().optional(),
  model_label: z.string().optional(),
  series_key: z.string().optional(),
  series_label: z.string().optional(),
  provider: z.string().optional(),
  cache_reporting_state: z.string().optional(),
  partial: z.boolean().optional(),
  risk_flags: z.array(z.string()).optional(),
}).passthrough();

const optionalUsageRowsSchema = z.preprocess(
  (value) => (value === null ? undefined : value),
  z.array(usageMetricRowSchema).optional(),
);

const windowSchema = z.object({
  timezone: z.string().optional(),
  utc_offset_seconds: z.coerce.number().optional(),
  next_day_start_at: z.coerce.number().optional(),
}).passthrough();

export const usageEnvelopeSchema = z.union([
  z.array(usageMetricRowSchema).transform((rows) => ({ rows })),
  z.object({
    rows: optionalUsageRowsSchema,
    usage: optionalUsageRowsSchema,
    data: optionalUsageRowsSchema,
    accounts: optionalUsageRowsSchema,
    window: windowSchema.optional(),
    effective_start_at: z.coerce.number().optional(),
    effective_until_at: z.coerce.number().optional(),
  }).passthrough().transform((value) => ({ ...value, rows: value.rows ?? value.usage ?? value.data ?? value.accounts ?? [] })),
]);

const usageBucketSchema = usageMetricRowSchema.extend({ bucket: z.coerce.number() });
const seriesDescriptorSchema = z.object({
  series_dimension: z.string().optional(),
  series_key: z.string(),
  series_label: z.string().optional(),
}).passthrough();
export const usageTimeseriesSchema = z.object({
  buckets: z.preprocess((value) => (value === null ? undefined : value), z.array(usageBucketSchema).optional()),
  model_series: optionalUsageRowsSchema,
  series: z.preprocess((value) => (value === null ? undefined : value), z.array(seriesDescriptorSchema).optional()),
}).passthrough().transform((value) => ({
  buckets: value.buckets ?? [],
  modelSeries: value.model_series ?? [],
  series: value.series ?? [],
}));

export const usageByModelSchema = z.union([
  z.array(usageMetricRowSchema),
  z.object({ models: optionalUsageRowsSchema }).passthrough().transform((value) => value.models ?? []),
]);

export const usageCacheSchema = z.object({
  summary: z.preprocess((value) => (value === null ? undefined : value), usageMetricRowSchema.optional()),
  stable_summary: z.preprocess((value) => (value === null ? undefined : value), usageMetricRowSchema.optional()),
  by_account: optionalUsageRowsSchema,
  by_model: optionalUsageRowsSchema,
  by_api_key: optionalUsageRowsSchema,
  by_account_model: optionalUsageRowsSchema,
  by_provider: optionalUsageRowsSchema,
  by_provider_model: optionalUsageRowsSchema,
  by_route: optionalUsageRowsSchema,
  by_route_account_model: optionalUsageRowsSchema,
  by_time_bucket: optionalUsageRowsSchema,
  window: windowSchema.optional(),
  effective_start_at: z.coerce.number().optional(),
}).passthrough();

const ranges = {
  today: { bucket: 3600 },
  604800: { bucket: 86400 },
  2592000: { bucket: 86400 },
} as const;

export async function fetchUsageDashboard(range: UsageRange, signal?: AbortSignal): Promise<UsageDashboard> {
  const definition = ranges[range];
  const now = Math.floor(Date.now() / 1000);
  const windowParams = range === 'today' ? undefined : { since: now - Number(range) };
  const bucketParams = range === 'today'
    ? { bucket: definition.bucket, series_dimension: 'model', series_limit: 6 }
    : { since: now - Number(range), bucket: definition.bucket, series_dimension: 'model', series_limit: 6 };
  const cacheParams = { bucket: definition.bucket, fields: FULL_CACHE_FIELDS };

  const [usageRaw, timeseriesRaw, modelsRaw, cacheRaw] = await Promise.all([
    get('/admin/usage', windowParams, { signal }),
    get('/admin/usage/timeseries', bucketParams, { signal }),
    get('/admin/usage/by-model', windowParams, { signal }),
    get('/admin/usage/cache', cacheParams, { signal }),
  ]);
  const usageWindow = parseApiResponse(usageEnvelopeSchema, usageRaw) as UsageEnvelope;
  const timeseries = parseApiResponse(usageTimeseriesSchema, timeseriesRaw) as {
    buckets: UsageBucket[]; modelSeries: UsageMetricRow[]; series: UsageSeriesDescriptor[];
  };
  return {
    rows: usageWindow.rows,
    buckets: timeseries.buckets,
    modelSeries: timeseries.modelSeries,
    series: timeseries.series,
    byModel: parseApiResponse(usageByModelSchema, modelsRaw) as UsageMetricRow[],
    cache: parseApiResponse(usageCacheSchema, cacheRaw) as UsageCacheReport,
    usageWindow,
  };
}

export async function resetUsageCacheStats() {
  return post('/admin/usage/cache/reset', {});
}
