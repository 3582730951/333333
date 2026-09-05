import { z } from 'zod';

const optionalFinite = z.preprocess(
  (value) => value == null || value === '' ? undefined : value,
  z.coerce.number().finite().optional(),
);
const optionalNonNegativeInteger = z.preprocess(
  (value) => value == null || value === '' ? undefined : value,
  z.coerce.number().int().nonnegative().optional(),
);
const optionalPositiveInteger = z.preprocess(
  (value) => value == null || value === '' ? undefined : value,
  z.coerce.number().int().positive().optional(),
);
const optionalString = z.preprocess(
  (value) => value == null ? undefined : value,
  z.string().optional(),
);
const optionalBoolean = z.preprocess(
  (value) => value == null ? undefined : value,
  z.boolean().optional(),
);

const planFamilySchema = z.enum(['unknown', 'free', 'plus', 'pro', 'business', 'enterprise', 'edu', 'api']).catch('unknown');
const seatTypeSchema = z.enum(['unknown', 'personal', 'business_standard', 'business_premium', 'legacy_codex', 'enterprise_standard']).catch('unknown');
const confidenceSchema = z.enum(['low', 'medium', 'high', 'unknown']).catch('unknown');
const flagsStateSchema = z.enum(['known', 'unknown', 'contradictory']).catch('unknown');

const planPresentationSchema = z.object({
  plan_family: planFamilySchema,
  seat_type: seatTypeSchema,
  plan_display_name: optionalString,
  seat_display_name: optionalString,
  combined: optionalString,
}).passthrough();

const entitlementEvidenceSchema = z.object({
  id: optionalString,
  source_kind: optionalString,
  plan_family: planFamilySchema,
  seat_type: seatTypeSchema,
  confidence: confidenceSchema,
  usage_multiplier_milli: optionalPositiveInteger,
  no_five_hour_limit: optionalBoolean,
  raw_plan_label: optionalString,
  observed_at: optionalNonNegativeInteger,
  expires_at: optionalNonNegativeInteger,
  flags_state: flagsStateSchema,
  freshness: z.enum(['fresh', 'stale', 'unknown']).catch('unknown'),
}).passthrough();

const entitlementPlanSchema = z.object({
  plan_family: planFamilySchema,
  seat_type: seatTypeSchema,
  confidence: confidenceSchema,
  usage_multiplier_milli: optionalPositiveInteger,
  no_five_hour_limit: optionalBoolean,
  flags_state: flagsStateSchema,
  reason: optionalString,
}).passthrough();

const upstreamQuotaWindowSchema = z.object({
  account_id: optionalString,
  provider: optionalString,
  model: optionalString,
  limiter_type: optionalString,
  source: optionalString,
  used_percent: optionalFinite,
  limit_tokens: optionalFinite,
  remaining_tokens: optionalFinite,
  limit_requests: optionalFinite,
  remaining_requests: optionalFinite,
  reset_at: optionalNonNegativeInteger,
  status: optionalString,
  updated_at: optionalNonNegativeInteger,
}).passthrough().superRefine((value, ctx) => {
  for (const key of ['raw', 'raw_json', 'headers', 'authorization', 'access_token', 'refresh_token', 'id_token', 'session_cookie']) {
    if (Object.prototype.hasOwnProperty.call(value, key)) {
      ctx.addIssue({ code: 'custom', message: `capacity quota window must not expose ${key}` });
    }
  }
});

const capacityEstimateSchema = z.object({
  account_id: optionalString,
  limiter_kind: optionalString,
  model_family: optionalString,
  service_tier: optionalString,
  cycle_start: optionalNonNegativeInteger,
  cycle_end: optionalNonNegativeInteger,
  used_ratio_ppm: optionalNonNegativeInteger,
  remaining_units: optionalFinite,
  unit_kind: optionalString,
  usd_equivalent_micro: optionalFinite,
  credits_remaining_milli: optionalFinite,
  method: optionalString,
  sample_count: optionalNonNegativeInteger,
  confidence: confidenceSchema,
  lower_bound_units: optionalFinite,
  upper_bound_units: optionalFinite,
  updated_at: optionalNonNegativeInteger,
}).passthrough();

const standardPriorEstimateSchema = z.object({
  model_family: optionalString,
  service_tier: optionalString,
  limit_micro_usd: optionalFinite,
  lower_micro_usd: optionalFinite,
  upper_micro_usd: optionalFinite,
  source_account_id: optionalString,
  source_sample_count: optionalNonNegativeInteger,
  source_confidence: confidenceSchema,
  source_updated_at: optionalNonNegativeInteger,
}).passthrough();

const standardPriorSchema = z.object({
  status: optionalString,
  method: optionalString,
  factor_milli: optionalPositiveInteger,
  role: optionalString,
  estimates: z.array(standardPriorEstimateSchema).default([]),
}).passthrough();

const valuationSchema = z.object({
  api_micro_usd_settled: optionalFinite,
  api_micro_usd_provisional: optionalFinite,
  chatgpt_milli_credits_settled: optionalFinite,
  chatgpt_milli_credits_provisional: optionalFinite,
  settled_events: optionalNonNegativeInteger,
  provisional_events: optionalNonNegativeInteger,
  unavailable_events: optionalNonNegativeInteger,
  updated_at: optionalNonNegativeInteger,
}).passthrough();

const valuationWindowSchema = z.object({
  from_at: optionalNonNegativeInteger,
  to_at: optionalNonNegativeInteger,
}).passthrough();

const labelsSchema = z.object({
  usd: optionalString,
  credits: optionalString,
}).passthrough();

export const capacityResponseSchema = z.object({
  account_id: optionalString,
  capacity_estimates: z.array(capacityEstimateSchema).default([]),
  upstream_quota_windows: z.array(upstreamQuotaWindowSchema).default([]),
  last_30d_valuation: valuationSchema.optional(),
  valuation_window: valuationWindowSchema.optional(),
  business_standard_5h_prior: standardPriorSchema.optional(),
  plan_presentation: planPresentationSchema.optional(),
  labels: labelsSchema.optional(),
  updated_at: optionalNonNegativeInteger,
  entitlement: z.object({
    plan_only: entitlementPlanSchema.optional(),
    current_evidence: entitlementEvidenceSchema.nullable().optional(),
    evidence: z.array(entitlementEvidenceSchema).default([]),
    conflict: z.boolean().catch(false),
    evidence_freshness: z.enum(['fresh', 'stale', 'unknown']).catch('unknown'),
    premium_fixture_status: optionalString,
    premium_fixture_mapping_version: optionalString,
  }).passthrough().default({ evidence: [], conflict: false, evidence_freshness: 'unknown' }),
}).passthrough().superRefine((value, ctx) => {
  const forbiddenCredentialKeys = ['access_token', 'refresh_token', 'id_token', 'session_cookie', 'authorization', 'cookie'];
  const walk = (candidate: unknown, path: Array<string | number>) => {
    if (Array.isArray(candidate)) {
      candidate.forEach((item, index) => walk(item, [...path, index]));
      return;
    }
    if (!candidate || typeof candidate !== 'object') return;
    for (const [key, nested] of Object.entries(candidate as Record<string, unknown>)) {
      if (forbiddenCredentialKeys.includes(key.toLowerCase())) {
        ctx.addIssue({ code: 'custom', path: [...path, key], message: `capacity response must not expose ${key}` });
      }
      walk(nested, [...path, key]);
    }
  };
  walk(value, []);
});

export type CapacityResponse = z.infer<typeof capacityResponseSchema>;
export type CapacityEntitlement = CapacityResponse['entitlement'];

export function parseCapacityResponse(value: unknown) {
  const parsed = capacityResponseSchema.safeParse(value);
  return parsed.success
    ? parsed.data
    : {
      error: '容量响应结构不可用',
      capacity_estimates: [],
      upstream_quota_windows: [],
      entitlement: { evidence: [], conflict: false, evidence_freshness: 'unknown' as const },
    };
}
