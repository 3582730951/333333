import { z } from 'zod';
import { del, get, patch, post } from '../../../api.js';
import { parseApiResponse } from '../../../api/contracts';
import type { ApiKeyCreateInput, ApiKeyRow } from '../model/keys';

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

export async function deleteAdminKey(hash: string) {
  return del(`/admin/api-keys/${encodeURIComponent(hash)}`);
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
