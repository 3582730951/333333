export interface AccountUsage {
  requests?: number;
  total_tokens?: number;
  prompt_tokens?: number;
  completion_tokens?: number;
  cached_tokens?: number;
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
  quarantine_until?: number;
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
