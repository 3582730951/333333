import { z } from 'zod';
import { del, get, patch, post } from '../../../api.js';
import { parseApiResponse } from '../../../api/contracts';
import type { UserCreateInput, UserRow, UserUpdateInput } from '../model/users';

const userSchema = z.object({
  id: z.string(),
  email: z.string(),
  name: z.string().optional(),
  role: z.string(),
  status: z.string(),
  created_at: z.number().optional(),
}).passthrough();

export const usersResponseSchema = z.union([
  z.null().transform(() => []),
  z.array(userSchema),
  z.object({ users: z.array(userSchema).optional(), rows: z.array(userSchema).optional() })
    .passthrough()
    .transform((value) => value.users ?? value.rows ?? []),
]);

export async function fetchUsers(signal?: AbortSignal): Promise<UserRow[]> {
  return parseApiResponse(usersResponseSchema, await get('/admin/users', undefined, { signal })) as UserRow[];
}

export async function createUser(input: UserCreateInput) {
  return post('/admin/users', input);
}

export async function updateUser({ id, values }: UserUpdateInput) {
  return patch(`/admin/users/${encodeURIComponent(id)}`, values);
}

export async function deleteUser(id: string) {
  return del(`/admin/users/${encodeURIComponent(id)}`);
}
