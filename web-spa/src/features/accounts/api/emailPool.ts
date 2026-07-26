import { z } from 'zod';
import { get, post, del } from '../../../api.js';
import { parseApiResponse } from '../../../api/contracts';

export interface EmailAccount {
  id: string;
  email: string;
  client_id?: string;
  status: string;
  group_name?: string;
  error_message?: string;
  last_used_at?: number;
  created_at: number;
  updated_at: number;
}

export interface EmailPoolListResponse {
  accounts: EmailAccount[];
  total: number;
  page: number;
  pageSize: number;
  counts: Record<string, number>;
}

export interface EmailPoolImportRequest {
  text: string;
  group_name?: string;
}

export interface EmailPoolImportResponse {
  imported: number;
  total: number;
  parse_errors?: string[];
}

export interface EmailPoolTestResponse {
  ok: boolean;
  email: string;
  error?: string;
  has_token?: boolean;
}

const emailAccountSchema = z.object({
  id: z.string(),
  email: z.string(),
  client_id: z.string().optional(),
  status: z.string(),
  group_name: z.string().optional(),
  error_message: z.string().optional(),
  last_used_at: z.coerce.number().optional(),
  created_at: z.coerce.number(),
  updated_at: z.coerce.number(),
}).passthrough();

export const emailPoolResponseSchema = z.object({
  accounts: z.array(emailAccountSchema).optional(),
  total: z.coerce.number().int().nonnegative().optional(),
  page: z.coerce.number().int().positive().optional(),
  pageSize: z.coerce.number().int().positive().optional(),
  counts: z.record(z.string(), z.coerce.number().int().nonnegative()).optional(),
}).passthrough().transform((value): EmailPoolListResponse => {
  const accounts = value.accounts ?? [];
  return {
    accounts,
    total: value.total ?? accounts.length,
    page: value.page ?? 1,
    pageSize: value.pageSize ?? Math.max(accounts.length, 1),
    counts: value.counts ?? {},
  };
});

export async function fetchEmailPool(params: {
  page?: number;
  pageSize?: number;
  search?: string;
  status?: string;
} = {}, signal?: AbortSignal): Promise<EmailPoolListResponse> {
  const searchParams = new URLSearchParams();
  if (params.page) searchParams.set('page', String(params.page));
  if (params.pageSize) searchParams.set('pageSize', String(params.pageSize));
  if (params.search) searchParams.set('search', params.search);
  if (params.status) searchParams.set('status', params.status);
  const qs = searchParams.toString();
  const response = await get(`/admin/email-pool${qs ? `?${qs}` : ''}`, undefined, { signal });
  return parseApiResponse(emailPoolResponseSchema, response);
}

export async function importEmailAccounts(data: EmailPoolImportRequest): Promise<EmailPoolImportResponse> {
  return post('/admin/email-pool/import', data);
}

export async function deleteEmailAccounts(ids: string[]): Promise<{ deleted: number; errors?: string[] }> {
  return del('/admin/email-pool', { ids });
}

export async function testEmailAccount(id: string): Promise<EmailPoolTestResponse> {
  return post(`/admin/email-pool/${encodeURIComponent(id)}/test`);
}
