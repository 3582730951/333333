export interface ApiKeyRow {
  key_hash?: string;
  hash?: string;
  label?: string;
  group_name?: string;
  user_group_id?: string;
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
  user_group_id?: string;
  force_model?: string;
  force_effort?: string;
  key_type?: string;
  expires_at?: number;
  [key: string]: unknown;
}

export interface ApiKeyUpdateInput {
  hash: string;
  label?: string;
  group_name?: string;
  user_group_id?: string;
  force_model?: string;
  force_effort?: string;
  enabled?: boolean;
  expires_at?: number;
}

export interface ApiKeyRoutingOptions {
  accountGroups: Array<{ name: string }>;
  userGroups: Array<{ id: string; name: string }>;
}
