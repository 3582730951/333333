import { z } from 'zod';
import { get, post } from '../../../api.js';
import { parseApiResponse } from '../../../api/contracts';
import type {
  UsageBucket, UsageCacheReport, UsageDashboard, UsageEnvelope, UsageMetricRow,
  UsageRange, UsageSeriesDescriptor,
} from '../model/usage';

export const FULL_CACHE_FIELDS = 'summary,by_account,by_model,by_api_key,by_account_model,by_route,by_route_account_model,by_time_bucket';

const numericKeys = [
  'requests', 'real_requests', 'hit_requests', 'request_hit_rate', 'token_hit_rate', 'real_token_hit_rate',
  'eligible_cache_hit_rate', 'cache_write_share', 'cache_read_share', 'prompt_tokens', 'completion_tokens', 'total_tokens',
  'cached_tokens', 'cache_input_tokens', 'cache_read_tokens', 'cache_creation_tokens', 'cache_miss_tokens',
  'cache_creation_5m_share', 'stable_prefix_bytes', 'cache_breakpoint_count', 'estimated_rate', 'bucket',
  'latest_user_auto_context_cache_control', 'latest_user_tail_cache_control', 'latest_user_tool_result_cache_control',
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
  risk_flags: z.array(z.string()).optional(),
  latest_user_cache_control: z.boolean().optional(),
}).passthrough();

const windowSchema = z.object({
  timezone: z.string().optional(),
  utc_offset_seconds: z.coerce.number().optional(),
  next_day_start_at: z.coerce.number().optional(),
}).passthrough();

export const usageEnvelopeSchema = z.union([
  z.array(usageMetricRowSchema).transform((rows) => ({ rows })),
  z.object({
    rows: z.array(usageMetricRowSchema).optional(),
    usage: z.array(usageMetricRowSchema).optional(),
    data: z.array(usageMetricRowSchema).optional(),
    accounts: z.array(usageMetricRowSchema).optional(),
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
  buckets: z.array(usageBucketSchema).optional(),
  model_series: z.array(usageMetricRowSchema).optional(),
  series: z.array(seriesDescriptorSchema).optional(),
}).passthrough().transform((value) => ({
  buckets: value.buckets ?? [],
  modelSeries: value.model_series ?? [],
  series: value.series ?? [],
}));

export const usageByModelSchema = z.union([
  z.array(usageMetricRowSchema),
  z.object({ models: z.array(usageMetricRowSchema).optional() }).passthrough().transform((value) => value.models ?? []),
]);

export const usageCacheSchema = z.object({
  summary: usageMetricRowSchema.optional(),
  by_account: z.array(usageMetricRowSchema).optional(),
  by_model: z.array(usageMetricRowSchema).optional(),
  by_api_key: z.array(usageMetricRowSchema).optional(),
  by_account_model: z.array(usageMetricRowSchema).optional(),
  by_route: z.array(usageMetricRowSchema).optional(),
  by_route_account_model: z.array(usageMetricRowSchema).optional(),
  by_time_bucket: z.array(usageMetricRowSchema).optional(),
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
