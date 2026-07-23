import { get, post, del } from '../../../api.js';

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

export async function fetchEmailPool(params: {
  page?: number;
  pageSize?: number;
  search?: string;
  status?: string;
} = {}): Promise<EmailPoolListResponse> {
  const searchParams = new URLSearchParams();
  if (params.page) searchParams.set('page', String(params.page));
  if (params.pageSize) searchParams.set('pageSize', String(params.pageSize));
  if (params.search) searchParams.set('search', params.search);
  if (params.status) searchParams.set('status', params.status);
  const qs = searchParams.toString();
  return get(`/admin/email-pool${qs ? `?${qs}` : ''}`);
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
