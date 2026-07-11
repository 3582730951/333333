import { z } from 'zod';
import { get } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import { usageTimeseriesSchema } from '../../observability/api/usage';
import type { UsageBucket } from '../../observability/model/usage';
import type { PortalUsageDashboard, PortalUsageRow } from '../model/usage';

const numeric = z.coerce.number().nonnegative().optional();
const portalUsageRowSchema = z.object({
  model: z.string(),
  model_key: z.string().optional(),
  model_label: z.string().optional(),
  requests: numeric,
  prompt_tokens: numeric,
  completion_tokens: numeric,
  total_tokens: numeric,
  cached_tokens: numeric,
  cache_input_tokens: numeric,
  cache_read_tokens: numeric,
  cache_creation_tokens: numeric,
}).passthrough();
export const portalUsageResponseSchema = z.union([
  z.array(portalUsageRowSchema),
  z.object({ usage: z.array(portalUsageRowSchema).optional(), rows: z.array(portalUsageRowSchema).optional() })
    .passthrough().transform((value) => value.usage ?? value.rows ?? []),
]);

function partialError(failures: unknown[]) {
  return createApiError({ code: 'PORTAL_USAGE_TIMESERIES_FAILED', userMessage: '用量摘要已加载，但趋势数据暂时不可用。', retryable: true, cause: failures });
}

async function fetchPortalUsageRows(signal?: AbortSignal): Promise<PortalUsageRow[]> {
  return parseApiResponse(portalUsageResponseSchema, await get('/user/usage', undefined, { signal })) as PortalUsageRow[];
}

async function fetchPortalUsageTimeseries(signal?: AbortSignal): Promise<UsageBucket[]> {
  const now = Math.floor(Date.now() / 1000);
  const value = parseApiResponse(usageTimeseriesSchema, await get('/user/usage/timeseries', { since: now - 7 * 86400, bucket: 86400 }, { signal }));
  return value.buckets;
}

export async function fetchPortalUsageDashboard(signal?: AbortSignal): Promise<PortalUsageDashboard> {
  const [rows, timeseries] = await Promise.allSettled([
    fetchPortalUsageRows(signal), fetchPortalUsageTimeseries(signal),
  ]);
  if (rows.status === 'rejected') throw rows.reason;
  return {
    rows: rows.value,
    buckets: timeseries.status === 'fulfilled' ? timeseries.value : [],
    timeseriesAvailable: timeseries.status === 'fulfilled',
    error: timeseries.status === 'rejected' ? partialError([timeseries.reason]) : null,
  };
}
