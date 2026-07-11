import type { ApiError } from '../../../model/contracts';
import type { SystemMetrics } from './system';
import type { UsageBucket, UsageCacheReport, UsageMetricRow } from './usage';

export interface AccountPoolSummary {
  total: number;
  active: number;
  quarantined: number;
  cooling: number;
  recheck: number;
  codex: number;
  claude: number;
  other: number;
}

export interface DashboardHealth {
  ok: boolean;
  [key: string]: unknown;
}

export interface RegistrationDay {
  date?: string;
  succeeded?: number;
  failed?: number;
  [key: string]: unknown;
}

export interface RegistrationStats {
  totals?: { success_rate?: number; succeeded?: number; failed?: number; [key: string]: unknown };
  by_day?: RegistrationDay[];
  [key: string]: unknown;
}

export interface DashboardCore {
  accountSummary: AccountPoolSummary;
  health: DashboardHealth | null;
  buckets: UsageBucket[];
  healthAvailable: boolean;
  timeseriesAvailable: boolean;
  error: ApiError | null;
}

export interface DashboardSecondary {
  registration: RegistrationStats | null;
  system: SystemMetrics | null;
  byModel: UsageMetricRow[];
  cache: UsageCacheReport | null;
  registrationAvailable: boolean;
  systemAvailable: boolean;
  modelAvailable: boolean;
  cacheAvailable: boolean;
  error: ApiError | null;
}
