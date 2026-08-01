import { z } from 'zod';
import { get, getResponse, post, put, del } from '../../../api.js';
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
    repository_path?: string;
    quickstart?: string[];
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

const optionalLegacyString = z.preprocess((value) => value == null ? undefined : value, z.string().optional());
const optionalLegacyNumber = z.preprocess((value) => value == null || value === '' ? undefined : value, z.coerce.number().optional());

const emailAccountSchema = z.object({
  id: z.string().min(1),
  email: z.string().min(1),
  client_id: optionalLegacyString,
  status: z.string().min(1).default('idle'),
  group_name: optionalLegacyString,
  error_message: optionalLegacyString,
  last_used_at: optionalLegacyNumber,
  // Pre-release responses did not consistently expose timestamps. A zero value
  // means "unknown" throughout the existing UI and preserves those rows.
  created_at: z.preprocess((value) => value == null || value === '' ? 0 : value, z.coerce.number()),
  updated_at: z.preprocess((value) => value == null || value === '' ? 0 : value, z.coerce.number()),
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
    repository_path: z.string().optional(),
    quickstart: z.array(z.string()).optional(),
    steps: z.array(z.string()).optional(),
    references: z.array(z.string()).optional(),
  }).optional(),
}).passthrough();

type UnknownRecord = Record<string, unknown>;

function isRecord(value: unknown): value is UnknownRecord {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function firstDefined(record: UnknownRecord, keys: string[]): unknown {
  for (const key of keys) {
    if (record[key] !== undefined) return record[key];
  }
  return undefined;
}

function normalizeLegacyEmailStatus(value: unknown): string {
  const normalized = String(value || '').trim().toLowerCase().replaceAll('-', '_');
  if (['', 'idle', 'unused', 'active', 'valid'].includes(normalized)) return 'idle';
  // `ready` was a user-visible state in the legacy UI ("可用"). Keep that
  // display semantic while the page still treats both ready and idle as
  // selectable capacity. New servers emit idle; old servers remain legible.
  if (['ready', 'available'].includes(normalized)) return 'ready';
  if (['in_use', 'inuse', 'busy', 'reserved', 'using', 'processing'].includes(normalized)) return 'in_use';
  if (['error', 'failed', 'invalid', 'disabled', 'dead'].includes(normalized)) return 'error';
  if (['used', 'consumed', 'done', 'completed'].includes(normalized)) return 'used';
  return normalized;
}

function normalizeLegacyEmailCounts(value: unknown): unknown {
  if (!isRecord(value)) return value;
  const counts: Record<string, number> = {};
  for (const [status, rawCount] of Object.entries(value)) {
    const count = Number(rawCount);
    if (!Number.isFinite(count) || count < 0) continue;
    const canonical = normalizeLegacyEmailStatus(status);
    counts[canonical] = (counts[canonical] || 0) + count;
  }
  return counts;
}

function normalizeLegacyEmailAccount(value: unknown): unknown {
  if (!isRecord(value)) return value;
  const normalized: UnknownRecord = { ...value };
  const aliases = [
    'account_id', 'accountId', 'address', 'clientId', 'state', 'groupName',
    'errorMessage', 'lastUsedAt', 'createdAt', 'updatedAt',
  ];
  for (const alias of aliases) delete normalized[alias];
  const canonical: Array<[string, unknown]> = [
    ['id', firstDefined(value, ['id', 'account_id', 'accountId'])],
    ['email', firstDefined(value, ['email', 'address'])],
    ['client_id', firstDefined(value, ['client_id', 'clientId'])],
    ['status', normalizeLegacyEmailStatus(firstDefined(value, ['status', 'state']))],
    ['group_name', firstDefined(value, ['group_name', 'groupName'])],
    ['error_message', firstDefined(value, ['error_message', 'errorMessage'])],
    ['last_used_at', firstDefined(value, ['last_used_at', 'lastUsedAt'])],
    ['created_at', firstDefined(value, ['created_at', 'createdAt']) ?? 0],
    ['updated_at', firstDefined(value, ['updated_at', 'updatedAt']) ?? 0],
  ];
  for (const [key, next] of canonical) {
    if (next == null || next === '') delete normalized[key];
    else normalized[key] = next;
  }
  // Timestamp schemas intentionally map missing values to zero.
  if (normalized.created_at === undefined) normalized.created_at = 0;
  if (normalized.updated_at === undefined) normalized.updated_at = 0;
  return normalized;
}

/**
 * Adapt only response shapes emitted by known pool releases. Unknown objects
 * (including API error payloads) deliberately remain invalid instead of being
 * displayed as an empty pool.
 */
export function normalizeEmailPoolResponse(value: unknown): unknown {
  if (Array.isArray(value)) {
    return { accounts: value.map(normalizeLegacyEmailAccount) };
  }
  if (!isRecord(value)) return value;

  const directRows = firstDefined(value, ['accounts', 'email_accounts', 'emailAccounts', 'rows', 'items', 'list']);
  if (Array.isArray(directRows)) {
    const pagination = isRecord(value.pagination) ? value.pagination : {};
    return {
      accounts: directRows.map(normalizeLegacyEmailAccount),
      total: firstDefined(value, ['total', 'count']) ?? firstDefined(pagination, ['total', 'count']),
      page: firstDefined(value, ['page', 'current_page', 'currentPage'])
        ?? firstDefined(pagination, ['page', 'current_page', 'currentPage']),
      pageSize: firstDefined(value, ['pageSize', 'page_size', 'limit'])
        ?? firstDefined(pagination, ['pageSize', 'page_size', 'per_page', 'perPage', 'limit']),
      counts: normalizeLegacyEmailCounts(firstDefined(value, ['counts', 'status_counts', 'statusCounts'])),
    };
  }

  // Some releases wrapped the page in { data: ... } or { result: ... }.
  const nested = firstDefined(value, ['data', 'result', 'payload']);
  if (Array.isArray(nested) || isRecord(nested)) {
    const normalized = normalizeEmailPoolResponse(nested);
    if (isRecord(normalized) && Array.isArray(normalized.accounts)) {
      return {
        ...normalized,
        total: normalized.total ?? firstDefined(value, ['total', 'count']),
        page: normalized.page ?? firstDefined(value, ['page', 'current_page', 'currentPage']),
        pageSize: normalized.pageSize ?? firstDefined(value, ['pageSize', 'page_size', 'limit']),
        counts: normalized.counts ?? normalizeLegacyEmailCounts(firstDefined(value, ['counts', 'status_counts', 'statusCounts'])),
      };
    }
  }
  return value;
}

const normalizedEmailPoolResponseSchema = z.object({
  accounts: z.array(emailAccountSchema).optional(),
  total: z.coerce.number().int().nonnegative().optional(),
  page: z.coerce.number().int().positive().optional(),
  pageSize: z.coerce.number().int().positive().optional(),
  counts: z.record(z.string(), z.coerce.number().int().nonnegative()).optional(),
}).passthrough().superRefine((value, context) => {
  if (!Array.isArray(value.accounts)) {
    context.addIssue({ code: 'custom', message: 'email pool rows are missing' });
  }
}).transform((value): EmailPoolListResponse => {
  const accounts = value.accounts ?? [];
  return {
    accounts,
    total: value.total ?? accounts.length,
    page: value.page ?? 1,
    pageSize: value.pageSize ?? Math.max(accounts.length, 1),
    counts: value.counts ?? {},
  };
});

export const emailPoolResponseSchema = z.preprocess(
  normalizeEmailPoolResponse,
  normalizedEmailPoolResponseSchema,
);

export async function fetchEmailPool(params: {
  page?: number;
  pageSize?: number;
  search?: string;
  status?: string;
} = {}, signal?: AbortSignal): Promise<EmailPoolListResponse> {
  const searchParams = new URLSearchParams();
  if (params.page) searchParams.set('page', String(params.page));
  if (params.pageSize) searchParams.set('pageSize', String(params.pageSize));
  if (params.pageSize) searchParams.set('page_size', String(params.pageSize));
  if (params.search) {
    searchParams.set('search', params.search);
    searchParams.set('q', params.search);
  }
  if (params.status) {
    searchParams.set('status', params.status);
    searchParams.set('state', params.status);
  }
  const qs = searchParams.toString();
  const response = await getResponse(`/admin/email-pool${qs ? `?${qs}` : ''}`, undefined, { signal });
  const bodyRequestId = isRecord(response.data) && typeof response.data.request_id === 'string'
    ? response.data.request_id
    : '';
  const rawHeaderRequestId = typeof response.headers?.get === 'function'
    ? response.headers.get('x-request-id')
    : response.headers?.['x-request-id'];
  const headerRequestId = typeof rawHeaderRequestId === 'string' ? rawHeaderRequestId : '';
  return parseApiResponse(emailPoolResponseSchema, response.data, bodyRequestId || headerRequestId);
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
