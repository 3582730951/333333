export type PortalValuation = {
  usage_event_id: string;
  valuation_kind: 'api_usd_equivalent' | 'chatgpt_credits' | string;
  catalog_id: string;
  catalog_effective_at: number;
  catalog_source_url: string;
  confidence: 'settled' | 'provisional' | 'unavailable' | string;
  amount_units: number | null;
  unit_scale: number;
  computed_at: number;
};

export type PortalUsageEvent = {
  usage_event_id: string;
  models: { requested: string; resolved: string; observed: string };
  service_tier: { requested: string; forwarded: string; observed: string; billed: string; reason: string };
  tokens: {
    input_total: number | null;
    input_uncached: number | null;
    cached_read: number | null;
    cache_write: number | null;
    output_total: number | null;
    output_reasoning: number | null;
    presence?: Record<string, boolean>;
  };
  settlement_state: string;
  estimated: boolean;
  integrity_error?: string;
  valuations: PortalValuation[];
  created_at: number;
  updated_at: number;
};

export type PortalUsagePage = {
  items: PortalUsageEvent[];
  next_cursor: string;
  has_more: boolean;
  from: number;
  to: number;
};

export type PortalQuota = {
  period: { from: number; to: number };
  accuracy: 'settled' | 'estimated' | 'partial';
  valuation: {
    api_micro_usd_settled: number;
    api_micro_usd_provisional: number;
    chatgpt_milli_credits_settled: number;
    chatgpt_milli_credits_provisional: number;
    settled_events: number;
    provisional_events: number;
    unavailable_events: number;
    updated_at: number;
  };
  catalog: null | {
    id: string;
    source_url: string;
    effective_at: number;
    fetched_at: number;
  };
  updated_at: number;
};

export type PortalSession = {
  id: string;
  current: boolean;
  user_agent: string;
  created_at: number;
  expires_at: number;
};
