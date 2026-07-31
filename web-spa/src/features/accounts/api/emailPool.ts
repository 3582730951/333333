import { z } from 'zod';
import { get, post, put, del } from '../../../api.js';
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

export interface CloudflareMailboxHealth {
  provider_key?: string;
  last_status?: string;
  last_checked_at?: number;
  latency_ms?: number;
  success_count?: number;
  failure_count?: number;
  consecutive_failures?: number;
  last_error_class?: string;
}

export interface CloudflareMailboxProfile {
  provider_key: string;
  display_name: string;
  api_url: string;
  domain: string;
  enabled: boolean;
  admin_token_configured: boolean;
  default_for_registration: boolean;
  default_for_team: boolean;
  health: CloudflareMailboxHealth;
  updated_at: number;
}

export interface CloudflareMailboxConfigResponse {
  profiles: CloudflareMailboxProfile[];
  defaults: { registration?: string; team?: string };
  deployment?: {
    recommended_adapter?: string;
    steps?: string[];
    references?: string[];
  };
}

export interface CloudflareMailboxSaveInput {
  provider_key?: string;
  display_name: string;
  api_url: string;
  domain: string;
  admin_token?: string;
  enabled: boolean;
  default_for_registration: boolean;
  default_for_team: boolean;
}

export interface CloudflareMailboxProbeResponse {
  ok: boolean;
  provider_key: string;
  domain: string;
  address_preview?: string;
  latency_ms: number;
  error_class?: string;
  message: string;
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

const cloudflareMailboxHealthSchema = z.object({
  provider_key: z.string().optional(),
  last_status: z.string().optional(),
  last_checked_at: z.coerce.number().optional(),
  latency_ms: z.coerce.number().optional(),
  success_count: z.coerce.number().optional(),
  failure_count: z.coerce.number().optional(),
  consecutive_failures: z.coerce.number().optional(),
  last_error_class: z.string().optional(),
}).passthrough();

const cloudflareMailboxProfileSchema = z.object({
  provider_key: z.string(),
  display_name: z.string(),
  api_url: z.string(),
  domain: z.string(),
  enabled: z.boolean(),
  admin_token_configured: z.boolean().optional().default(false),
  default_for_registration: z.boolean().optional().default(false),
  default_for_team: z.boolean().optional().default(false),
  health: cloudflareMailboxHealthSchema.optional().default({}),
  updated_at: z.coerce.number().optional().default(0),
}).passthrough();

export const cloudflareMailboxConfigSchema = z.object({
  profiles: z.array(cloudflareMailboxProfileSchema).optional().default([]),
  defaults: z.object({
    registration: z.string().optional(),
    team: z.string().optional(),
  }).optional().default({}),
  deployment: z.object({
    recommended_adapter: z.string().optional(),
    steps: z.array(z.string()).optional(),
    references: z.array(z.string()).optional(),
  }).optional(),
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

export async function fetchCloudflareMailboxConfig(signal?: AbortSignal): Promise<CloudflareMailboxConfigResponse> {
  const response = await get('/admin/email-pool/cloudflare', undefined, { signal });
  return parseApiResponse(cloudflareMailboxConfigSchema, response) as CloudflareMailboxConfigResponse;
}

export async function saveCloudflareMailboxProfile(input: CloudflareMailboxSaveInput): Promise<{
  saved: boolean;
  provider_key: string;
  domain: string;
  admin_token_configured: boolean;
}> {
  return put('/admin/email-pool/cloudflare', input);
}

export async function testCloudflareMailboxProfile(input: Partial<CloudflareMailboxSaveInput> & { provider_key?: string }): Promise<CloudflareMailboxProbeResponse> {
  return post('/admin/email-pool/cloudflare/test', input);
}

export async function deleteCloudflareMailboxProfile(providerKey: string): Promise<{ deleted: boolean }> {
  return del('/admin/email-pool/cloudflare', { provider_key: providerKey });
}
