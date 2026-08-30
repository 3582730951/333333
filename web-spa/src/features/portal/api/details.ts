import { z } from 'zod';
import { del, get } from '../../../api.js';
import { parseApiResponse } from '../../../api/contracts';
import type { PortalQuota, PortalSession, PortalUsagePage } from '../model/details';

const nullableCount = z.number().int().nonnegative().nullable();
const valuationSchema = z.object({
  usage_event_id: z.string(),
  valuation_kind: z.string(),
  catalog_id: z.string(),
  catalog_effective_at: z.number().int(),
  catalog_source_url: z.string(),
  confidence: z.string(),
  amount_units: nullableCount,
  unit_scale: z.number().int().positive(),
  computed_at: z.number().int(),
}).passthrough();

const usageEventSchema = z.object({
  usage_event_id: z.string(),
  models: z.object({ requested: z.string(), resolved: z.string(), observed: z.string() }),
  service_tier: z.object({
    requested: z.string(), forwarded: z.string(), observed: z.string(), billed: z.string(), reason: z.string(),
  }),
  tokens: z.object({
    input_total: nullableCount,
    input_uncached: nullableCount,
    cached_read: nullableCount,
    cache_write: nullableCount,
    output_total: nullableCount,
    output_reasoning: nullableCount,
    presence: z.record(z.string(), z.boolean()).optional(),
  }).passthrough(),
  settlement_state: z.string(),
  estimated: z.boolean(),
  integrity_error: z.string().optional(),
  valuations: z.array(valuationSchema),
  created_at: z.number().int(),
  updated_at: z.number().int(),
}).passthrough();

const usagePageSchema = z.object({
  items: z.array(usageEventSchema),
  next_cursor: z.string(),
  has_more: z.boolean(),
  from: z.number().int(),
  to: z.number().int(),
});

const quotaSchema = z.object({
  period: z.object({ from: z.number().int(), to: z.number().int() }),
  accuracy: z.enum(['settled', 'estimated', 'partial']),
  valuation: z.object({
    api_micro_usd_settled: z.number().int(),
    api_micro_usd_provisional: z.number().int(),
    chatgpt_milli_credits_settled: z.number().int(),
    chatgpt_milli_credits_provisional: z.number().int(),
    settled_events: z.number().int(),
    provisional_events: z.number().int(),
    unavailable_events: z.number().int(),
    updated_at: z.number().int(),
  }),
  catalog: z.object({
    id: z.string(), source_url: z.string(), effective_at: z.number().int(), fetched_at: z.number().int(),
  }).passthrough().nullable(),
  updated_at: z.number().int(),
}).passthrough();

const sessionsSchema = z.object({
  sessions: z.array(z.object({
    id: z.string(), current: z.boolean(), user_agent: z.string(), created_at: z.number().int(), expires_at: z.number().int(),
  })),
});

export type PortalUsageFilters = {
  cursor?: string;
  from?: number;
  to?: number;
  model?: string;
  service_tier?: 'default' | 'fast' | '';
  limit?: number;
};

export async function fetchPortalUsageEvents(filters: PortalUsageFilters = {}, signal?: AbortSignal): Promise<PortalUsagePage> {
  const value = await get('/user/usage/events', filters, { signal });
  return parseApiResponse(usagePageSchema, value) as PortalUsagePage;
}

export async function fetchPortalQuota(signal?: AbortSignal): Promise<PortalQuota> {
  return parseApiResponse(quotaSchema, await get('/user/quota', undefined, { signal })) as PortalQuota;
}

export async function fetchPortalSessions(signal?: AbortSignal): Promise<PortalSession[]> {
  return parseApiResponse(sessionsSchema, await get('/user/sessions', undefined, { signal })).sessions as PortalSession[];
}

export async function revokePortalSession(id: string) {
  return del(`/user/sessions/${encodeURIComponent(id)}`);
}
