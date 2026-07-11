export interface ApiKeyRow {
  key_hash?: string;
  hash?: string;
  label?: string;
  group_name?: string;
  force_model?: string;
  force_effort?: string;
  key_type?: string;
  enabled?: boolean;
  created_at?: number;
  expires_at?: number;
  last_used_at?: number;
  secret?: string;
  [key: string]: unknown;
}

export interface ApiKeyCreateInput {
  label?: string;
  group_name?: string;
  force_model?: string;
  force_effort?: string;
  key_type?: string;
  expires_at?: number;
  [key: string]: unknown;
}
