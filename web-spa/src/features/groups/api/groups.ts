import { get } from '../../../api.js';

export type UserGroup = {
  id?: string;
  name: string;
  pinned_egress_no_fallback?: boolean;
  gpt_safety_buffering_session_rollover_enabled?: boolean;
  gpt_text_refusal_session_rollover_enabled?: boolean;
  dynamic_pool_balance_enabled?: boolean;
  dynamic_pool_balance_rpm_threshold?: number;
  egress_rpm_balance_enabled?: boolean;
  egress_rpm_balance_threshold?: number;
  egress_rpm_balance_egress_ids?: string[];
  [key: string]: unknown;
};

function rowsOf(value: unknown, keys: string[]): any[] {
  if (Array.isArray(value)) return value;
  if (!value || typeof value !== 'object') return [];
  const record = value as Record<string, unknown>;
  for (const key of keys) {
    if (Array.isArray(record[key])) return record[key] as any[];
  }
  return [];
}

export async function fetchAccountGroups(signal?: AbortSignal) {
  return rowsOf(await get('/admin/groups', undefined, { signal }), ['groups']);
}

export async function fetchUserGroups(signal?: AbortSignal) {
  return rowsOf(await get('/admin/user-groups', undefined, { signal }), ['user_groups']);
}

export async function fetchGroupInstructions(signal?: AbortSignal) {
  return rowsOf(await get('/admin/model-instructions', undefined, { signal }), ['files']);
}

export async function fetchGroupSuperSkills(signal?: AbortSignal) {
  return rowsOf(await get('/admin/super-instruct/skills', undefined, { signal }), ['skills']);
}

export async function fetchGroupEgresses(signal?: AbortSignal) {
  return rowsOf(await get('/admin/egress-profiles', undefined, { signal }), ['profiles', 'egress_profiles']);
}

export async function fetchGroupProviders(signal?: AbortSignal) {
  return rowsOf(await get('/admin/providers', undefined, { signal }), ['providers']);
}

export async function fetchGroupModels(signal?: AbortSignal) {
  return rowsOf(await get('/admin/models', undefined, { signal }), ['models']);
}
