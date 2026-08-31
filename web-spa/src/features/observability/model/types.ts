import type { PlanPresentation } from '../../accounts/model/planFormatter';

export interface QuotaRow {
  account_id: string;
  label?: string;
  provider?: string;
  plan_type?: string;
  plan_presentation?: PlanPresentation;
  model?: string;
  oauth_rate_limit_tier?: string;
  limiter_type?: string;
  status?: string;
  reset_at?: number;
  used_percent?: number;
  secondary_7d_used_pct?: number;
  remaining_tokens?: number;
  quota_summary?: Record<string, any>;
  [key: string]: unknown;
}

export interface CFEventRow {
  id?: string | number;
  created_at?: number;
  status?: number | string;
  category?: string;
  message?: string;
  account_id?: string;
  egress_id?: string;
  cf_ray?: string;
  [key: string]: unknown;
}

export interface AuditRow {
  id?: string | number;
  created_at?: number;
  account_id?: string;
  account_label?: string;
  action?: string;
  state?: string;
  reason?: string;
  detail?: string;
  [key: string]: unknown;
}
