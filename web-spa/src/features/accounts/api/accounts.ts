import { z } from 'zod';
import { get } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import type { AccountGroup, AccountRow, AccountsBundle, AccountsPageParams } from '../model/types';

const accountSchema = z.object({
  id: z.string(),
  label: z.string().optional(),
  email: z.string().optional(),
  provider: z.string().optional(),
  status: z.string().optional(),
  group_name: z.string().optional(),
  plan_type: z.string().optional(),
  quarantine_until: z.number().optional(),
  usage: z.record(z.string(), z.unknown()).nullable().optional(),
}).passthrough();

export const accountsResponseSchema = z.union([
  z.array(accountSchema).transform((rows) => ({ rows, total: rows.length })),
  z.object({
    accounts: z.array(accountSchema).optional(),
    rows: z.array(accountSchema).optional(),
    total: z.coerce.number().int().nonnegative().optional(),
  }).passthrough().transform((value) => {
    const rows = value.accounts ?? value.rows ?? [];
    return { rows, total: value.total ?? rows.length };
  }),
]);

const groupSchema = z.object({ name: z.string() }).passthrough();
export const accountGroupsResponseSchema = z.union([
  z.array(groupSchema),
  z.object({ groups: z.array(groupSchema).optional() }).passthrough().transform((value) => value.groups ?? []),
]);

export async function fetchAccountsPage(params: AccountsPageParams, signal?: AbortSignal) {
  const response = await get('/admin/accounts', params, { signal });
  return parseApiResponse(accountsResponseSchema, response) as { rows: AccountRow[]; total: number };
}

export async function fetchAccountGroups(signal?: AbortSignal) {
  const response = await get('/admin/groups', undefined, { signal });
  return parseApiResponse(accountGroupsResponseSchema, response) as AccountGroup[];
}

export async function fetchAccountsBundle(params: AccountsPageParams, signal?: AbortSignal): Promise<AccountsBundle> {
  const [accounts, groups] = await Promise.allSettled([
    fetchAccountsPage(params, signal),
    fetchAccountGroups(signal),
  ]);
  if (accounts.status === 'rejected') throw accounts.reason;
  if (groups.status === 'fulfilled') return { ...accounts.value, groups: groups.value, error: null };
  const secondary = createApiError({
    code: 'GROUP_OPTIONS_FAILED',
    userMessage: '账号已加载，但分组选项暂时不可用。',
    retryable: true,
    cause: groups.reason,
  });
  return { ...accounts.value, groups: [], error: secondary };
}
