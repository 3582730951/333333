import { z } from 'zod';
import { del, get, patch, post } from '../../../api.js';
import { parseApiResponse } from '../../../api/contracts';
import type { ApiKeyCreateInput, ApiKeyRoutingOptions, ApiKeyRow, ApiKeyUpdateInput } from '../model/keys';

const keySchema = z.object({
  key_hash: z.string().optional(),
  hash: z.string().optional(),
  label: z.string().optional(),
  enabled: z.boolean().optional(),
  created_at: z.number().optional(),
}).passthrough();

export const keysResponseSchema = z.union([
  z.array(keySchema),
  z.object({ keys: z.array(keySchema).optional(), rows: z.array(keySchema).optional() })
    .passthrough()
    .transform((value) => value.keys ?? value.rows ?? []),
]);

export async function fetchAdminKeys(signal?: AbortSignal): Promise<ApiKeyRow[]> {
  return parseApiResponse(keysResponseSchema, await get('/admin/api-keys', undefined, { signal })) as ApiKeyRow[];
}

export async function createAdminKey(input: ApiKeyCreateInput) {
  return post('/admin/api-keys', input);
}

export async function updateAdminKey(input: ApiKeyUpdateInput): Promise<ApiKeyRow> {
  const { hash, ...values } = input;
  return patch(`/admin/api-keys/${encodeURIComponent(hash)}`, values) as Promise<ApiKeyRow>;
}

export async function deleteAdminKey(hash: string) {
  return del(`/admin/api-keys/${encodeURIComponent(hash)}`);
}

const accountGroupsSchema = z.union([
  z.array(z.object({ name: z.string() }).passthrough()),
  z.object({ groups: z.array(z.object({ name: z.string() }).passthrough()).optional() })
    .passthrough()
    .transform((value) => value.groups ?? []),
]);

const userGroupsSchema = z.union([
  z.array(z.object({ id: z.string(), name: z.string() }).passthrough()),
  z.object({ user_groups: z.array(z.object({ id: z.string(), name: z.string() }).passthrough()).optional() })
    .passthrough()
    .transform((value) => value.user_groups ?? []),
]);

export async function fetchAdminKeyRoutingOptions(signal?: AbortSignal): Promise<ApiKeyRoutingOptions> {
  const [accountGroups, userGroups] = await Promise.allSettled([
    Promise.resolve(get('/admin/groups', undefined, { signal }))
      .then((value) => parseApiResponse(accountGroupsSchema, value)),
    Promise.resolve(get('/admin/user-groups', undefined, { signal }))
      .then((value) => parseApiResponse(userGroupsSchema, value)),
  ]);
  if (accountGroups.status === 'rejected' && userGroups.status === 'rejected') throw accountGroups.reason;
  return {
    accountGroups: accountGroups.status === 'fulfilled' ? accountGroups.value : [],
    userGroups: userGroups.status === 'fulfilled' ? userGroups.value : [],
  } as ApiKeyRoutingOptions;
}

export async function fetchPortalKeys(signal?: AbortSignal): Promise<ApiKeyRow[]> {
  return parseApiResponse(keysResponseSchema, await get('/user/api-keys', undefined, { signal })) as ApiKeyRow[];
}

export async function createPortalKey(input: ApiKeyCreateInput) {
  return post('/user/api-keys', input);
}

export async function updatePortalKey(input: { hash: string; enabled: boolean }) {
  return patch(`/user/api-keys/${encodeURIComponent(input.hash)}`, { enabled: input.enabled });
}

export async function deletePortalKey(hash: string) {
  return del(`/user/api-keys/${encodeURIComponent(hash)}`);
}
