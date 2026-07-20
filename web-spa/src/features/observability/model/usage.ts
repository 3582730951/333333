export type UsageRange = 'today' | 604800 | 2592000;

export interface UsageMetricRow {
  account_id?: string;
  label?: string;
  model?: string;
  model_key?: string;
  model_label?: string;
  series_key?: string;
  series_label?: string;
  provider?: string;
  cache_reporting_state?: string;
  partial?: boolean;
  api_key_hash_prefix?: string;
  route_key_hash_prefix?: string;
  route_class?: string;
  affinity_source?: string;
  prompt_cache_key_source?: string;
  stable_prefix_source?: string;
  stable_prefix_reason?: string;
  retention_effective?: string;
  retention_source?: string;
  claude_cache_ttl?: string;
  requests?: number;
  real_requests?: number;
  hit_requests?: number;
  request_hit_rate?: number;
  token_hit_rate?: number;
  real_token_hit_rate?: number;
  eligible_cache_hit_rate?: number;
  cache_write_share?: number;
  cache_read_share?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  cached_tokens?: number;
  cache_input_tokens?: number;
  cache_read_tokens?: number;
  cache_creation_tokens?: number;
  cache_creation_reported_requests?: number;
  cache_miss_tokens?: number;
  cache_creation_5m_share?: number;
  stable_prefix_bytes?: number;
  cache_breakpoint_count?: number;
  latest_user_cache_control?: number;
  latest_user_auto_context_cache_control?: number;
  latest_user_tail_cache_control?: number;
  latest_user_tool_result_cache_control?: number;
  estimated_rate?: number;
  risk_flags?: string[];
  bucket?: number;
  [key: string]: unknown;
}

export interface UsageBucket extends UsageMetricRow {
  bucket: number;
}

export interface UsageSeriesDescriptor {
  series_dimension?: string;
  series_key: string;
  series_label?: string;
  [key: string]: unknown;
}

export interface UsageWindow {
  timezone?: string;
  utc_offset_seconds?: number;
  next_day_start_at?: number;
  [key: string]: unknown;
}

export interface UsageEnvelope {
  rows: UsageMetricRow[];
  window?: UsageWindow;
  effective_start_at?: number;
  effective_until_at?: number;
  [key: string]: unknown;
}

export interface UsageCacheReport {
  summary?: UsageMetricRow;
  stable_summary?: UsageMetricRow;
  by_account?: UsageMetricRow[];
  by_model?: UsageMetricRow[];
  by_api_key?: UsageMetricRow[];
  by_account_model?: UsageMetricRow[];
  by_provider?: UsageMetricRow[];
  by_provider_model?: UsageMetricRow[];
  by_route?: UsageMetricRow[];
  by_route_account_model?: UsageMetricRow[];
  by_time_bucket?: UsageMetricRow[];
  window?: UsageWindow;
  effective_start_at?: number;
  [key: string]: unknown;
}

export interface UsageDashboard {
  rows: UsageMetricRow[];
  buckets: UsageBucket[];
  modelSeries: UsageMetricRow[];
  series: UsageSeriesDescriptor[];
  byModel: UsageMetricRow[];
  cache: UsageCacheReport;
  usageWindow: UsageEnvelope;
}
