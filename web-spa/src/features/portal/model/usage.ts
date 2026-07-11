import type { ApiError } from '../../../model/contracts';
import type { UsageBucket } from '../../observability/model/usage';

export interface PortalUsageRow {
  model: string;
  model_key?: string;
  model_label?: string;
  requests?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  total_tokens?: number;
  cached_tokens?: number;
  cache_input_tokens?: number;
  cache_read_tokens?: number;
  cache_creation_tokens?: number;
  [key: string]: unknown;
}

export interface PortalUsageDashboard {
  rows: PortalUsageRow[];
  buckets: UsageBucket[];
  timeseriesAvailable: boolean;
  error: ApiError | null;
}
