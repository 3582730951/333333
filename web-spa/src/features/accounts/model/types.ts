export interface AccountUsage {
  requests?: number;
  total_tokens?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  cached_tokens?: number;
  [key: string]: unknown;
}

export interface AccountModelCapability {
  model_slug?: string;
  availability_state?: 'verified' | 'unverified' | 'unsupported' | string;
  context_1m_state?: 'supported' | 'unsupported' | 'unknown' | string;
  context_1m_source?: string;
  native_context_window?: number;
  native_max_context_window?: number;
  source?: string;
  [key: string]: unknown;
}

export interface AccountRow {
  id: string;
  label?: string;
  email?: string;
  provider?: string;
  status?: string;
  group_name?: string;
  plan_type?: string;
  auth_method?: 'oauth' | 'access_token' | 'api_key' | string;
  billing_mode?: 'subscription' | 'pay_as_you_go' | string;
  api_key_present?: boolean;
  ignore_rate_limit_controls?: boolean;
  quarantine_until?: number;
  quarantine_reason?: string;
  capabilities?: AccountModelCapability[];
  usage?: AccountUsage | null;
  [key: string]: unknown;
}

export interface AccountGroup {
  name: string;
  [key: string]: unknown;
}

export interface AccountsPageParams {
  page: number;
  pageSize: number;
  search: string;
}

export interface AccountsBundle {
  rows: AccountRow[];
  total: number;
  groups: AccountGroup[];
  error: Error | null;
}
