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
}

export interface RegistrationStrategyInput {
  strategy: RegistrationCountryStrategy;
  manualCountry: string;
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
  return String(method || fallback || 'protocol_v2').trim().toLowerCase();
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
