import type { ApiError } from '../../../model/contracts';

export type RegistrationIdentityMode = 'phone' | 'email';
export type RegistrationCountryStrategy = 'auto' | 'manual';

export interface RegistrationJob {
  id?: string;
  method?: string;
  identity_mode?: string;
  group_name?: string;
  egress_id?: string;
  status?: string;
  total?: number;
  succeeded?: number;
  failed?: number;
  created_at?: number;
  [key: string]: unknown;
}

export interface RegistrationReadiness {
  ready?: boolean;
  providers?: Record<string, number>;
  blockers?: string[];
  provider_error?: string;
  policy_error?: string;
  pool?: Record<string, unknown>;
  [key: string]: unknown;
}

export interface RegistrationDashboard {
  jobs: RegistrationJob[];
  readiness: RegistrationReadiness | null;
  readinessError: ApiError | null;
}

export interface RegistrationPool {
  id: string;
  name?: string;
  purpose?: string;
  members?: unknown[];
  [key: string]: unknown;
}

export interface RegistrationGroup {
  name: string;
  [key: string]: unknown;
}

export type RegistrationProviderOption = string | { label: string; value: string; [key: string]: unknown };

export interface RegistrationProviderOptions {
  sms: RegistrationProviderOption[];
  mailbox: RegistrationProviderOption[];
  captcha: RegistrationProviderOption[];
}

export interface RegistrationOptions {
  groups: RegistrationGroup[];
  pools: RegistrationPool[];
  providers: RegistrationProviderOptions;
  error: ApiError | null;
}

export interface RegistrationCountry {
  isoCode: string;
  name: string;
  nameZh: string;
  [key: string]: unknown;
}

export interface RegistrationStrategyConfig {
  strategy: RegistrationCountryStrategy;
  manualCountry: string;
  defaultMethod: string;
  minPrice: number;
  maxPrice: number;
}

export interface RegistrationStrategyInput {
  strategy: RegistrationCountryStrategy;
  manualCountry: string;
  minPrice: number;
  maxPrice: number;
}

export interface SMSMarketCandidate {
  provider: string;
  service: string;
  country_id: string;
  country_iso: string;
  country_name?: string;
  price: number;
  inventory: number;
  provider_rank: number;
  balance: number;
  fetched_at: number;
  attempts: number;
  succeeded: number;
  success_rate: number;
  score: number;
  eligible: boolean;
  selection_basis: 'historical_success_rate' | 'community_cold_start' | string;
}

export interface SMSMarketSnapshot {
  items: SMSMarketCandidate[];
  min_price: number;
  max_price: number;
  preferred_countries: string[];
  cold_start_policy: string;
  history_window_days: number;
  minimum_history_samples: number;
  refresh_interval_seconds: number;
  last_refreshed_at: number;
  stale: boolean;
  refreshed_rows: number;
  warning: string;
}

export interface RegistrationStartInput {
  count: number;
  group_name: string;
  method: string;
  registration_egress_pool_id: string;
  sms_provider: string;
  mailbox_provider: string;
  identity_mode: RegistrationIdentityMode;
  country: string;
}

export type RegistrationBlockerCode =
  | 'provider_error'
  | 'registration_uninitialized'
  | 'mailbox_missing'
  | 'email_otp_missing'
  | 'sms_missing';

export interface RegistrationBlocker {
  code: RegistrationBlockerCode;
  detail?: string;
}

export function normalizeRegisterMethod(method: unknown, fallback = 'protocol_v2'): string {
  const normalized = String(method || fallback || 'protocol_v2').trim().toLowerCase().replaceAll('-', '_');
  if (['email', 'email_otp', 'email_register', 'protocol2', 'protocol_v_2'].includes(normalized)) return 'protocol_v2';
  if (['turbo', 'turbo_gpt', 'turbo_gpt_register', 'playwright', 'browser3', 'browser_v_3'].includes(normalized)) return 'browser_v3';
  return normalized;
}

export function lockedIdentityForMethod(method: unknown): RegistrationIdentityMode | '' {
  switch (normalizeRegisterMethod(method)) {
    case 'node':
    case 'browser':
      return 'phone';
    case 'protocol_v2':
    case 'browser_v3':
      return 'email';
    default:
      return '';
  }
}

export function methodUsesSMSCountry(method: unknown, identityMode: RegistrationIdentityMode): boolean {
  const value = normalizeRegisterMethod(method);
  return value === 'node' || value === 'browser' || value === 'browser_v3' || (value === 'protocol' && identityMode === 'phone');
}

export function readinessProviderCount(readiness: RegistrationReadiness | null, key: string): number {
  return Number(readiness?.providers?.[key] || 0);
}

export function manualStartBlockers(
  readiness: RegistrationReadiness | null,
  identityMode: RegistrationIdentityMode,
  method: unknown,
): RegistrationBlocker[] {
  if (!readiness) return [];
  const blockers: RegistrationBlocker[] = [];
  if (readiness.provider_error) blockers.push({ code: 'provider_error', detail: readiness.provider_error });
  if ((readiness.blockers || []).some((blocker) => String(blocker).includes('注册子系统'))) blockers.push({ code: 'registration_uninitialized' });
  const value = normalizeRegisterMethod(method);
  if (value === 'protocol' && identityMode === 'email' && readinessProviderCount(readiness, 'mailbox') < 1) blockers.push({ code: 'mailbox_missing' });
  if (['protocol_v2', 'browser_v3'].includes(value) && readinessProviderCount(readiness, 'email_otp') < 1) blockers.push({ code: 'email_otp_missing' });
  if ((value === 'node' || value === 'browser' || (value === 'protocol' && identityMode === 'phone')) && readinessProviderCount(readiness, 'sms') < 1) blockers.push({ code: 'sms_missing' });
  return blockers.filter((blocker, index, all) => all.findIndex((candidate) => candidate.code === blocker.code && candidate.detail === blocker.detail) === index);
}
