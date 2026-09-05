import { describe, expect, it } from 'vitest';

import { capacityResponseSchema, parseCapacityResponse } from '../src/features/accounts/model/capacity';
import { formatPlanLabel } from '../src/features/accounts/model/planFormatter';

const validCapacityResponse = {
  account_id: 'acct_fixture',
  updated_at: 1_783_125_600,
  plan_presentation: {
    plan_family: 'business',
    seat_type: 'business_premium',
    plan_display_name: 'Team',
    seat_display_name: 'Premium (5×)',
    combined: 'Team (5×)',
  },
  upstream_quota_windows: [{
    account_id: 'acct_fixture',
    provider: 'codex',
    limiter_type: '7d',
    used_percent: 29,
    remaining_tokens: 123_456,
    reset_at: 1_783_730_400,
    updated_at: 1_783_125_600,
    status: 'ok',
  }],
  last_30d_valuation: {
    api_micro_usd_settled: 1_250_000,
    chatgpt_milli_credits_settled: 2_500,
    settled_events: 2,
  },
  valuation_window: { from_at: 1_780_533_600, to_at: 1_783_125_600 },
  business_standard_5h_prior: { status: 'suppressed_for_premium_5x', estimates: [] },
  entitlement: {
    plan_only: { plan_family: 'business', seat_type: 'unknown', confidence: 'low', flags_state: 'unknown' },
    current_evidence: {
      source_kind: 'quota_metadata',
      raw_plan_label: 'self_serve_business_prolite',
      plan_family: 'business',
      seat_type: 'business_premium',
      confidence: 'high',
      usage_multiplier_milli: 5000,
      no_five_hour_limit: true,
      observed_at: 1_783_125_600,
      expires_at: 1_783_212_000,
      flags_state: 'known',
      freshness: 'fresh',
    },
    evidence: [],
    conflict: false,
    evidence_freshness: 'fresh',
  },
};

describe('capacity response contract', () => {
  it('requires the backend presentation before rendering Team (5×)', () => {
    expect(formatPlanLabel('self_serve_business_prolite')).toBe('Business / Team');
    expect(formatPlanLabel('self_serve_business_prolite', {
      plan_family: 'business', seat_type: 'business_premium', combined: 'Team (5×)',
    })).toBe('Team (5×)');
  });

  it('accepts typed actual, valuation, and evidence layers', () => {
    const parsed = capacityResponseSchema.safeParse(validCapacityResponse);
    expect(parsed.success).toBe(true);
    if (!parsed.success) return;
    expect(parsed.data.plan_presentation?.combined).toBe('Team (5×)');
    expect(parsed.data.upstream_quota_windows[0]?.used_percent).toBe(29);
    expect(parsed.data.entitlement.current_evidence?.usage_multiplier_milli).toBe(5000);
  });

  it('accepts backend nulls for unknown entitlement flags', () => {
    const backendResponse = {
      ...validCapacityResponse,
      entitlement: {
        ...validCapacityResponse.entitlement,
        plan_only: {
          ...validCapacityResponse.entitlement.plan_only,
          usage_multiplier_milli: null,
          no_five_hour_limit: null,
        },
        current_evidence: {
          ...validCapacityResponse.entitlement.current_evidence,
          usage_multiplier_milli: null,
          no_five_hour_limit: null,
          payload_redacted: null,
        },
      },
    };
    expect(parseCapacityResponse(backendResponse)).not.toHaveProperty('error');
  });

  it('rejects raw rate-limit JSON and malformed evidence multiplier', () => {
    const unsafe = {
      ...validCapacityResponse,
      upstream_quota_windows: [{ ...validCapacityResponse.upstream_quota_windows[0], raw: '{"authorization":"secret"}' }],
      entitlement: {
        ...validCapacityResponse.entitlement,
        current_evidence: { ...validCapacityResponse.entitlement.current_evidence, usage_multiplier_milli: 5000.5 },
      },
    };
    expect(capacityResponseSchema.safeParse(unsafe).success).toBe(false);
    expect(parseCapacityResponse(unsafe)).toMatchObject({
      error: '容量响应结构不可用',
      upstream_quota_windows: [],
    });
  });
});
