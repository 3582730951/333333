import { z } from 'zod';
import api from '../../../api.js';
import { get } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import type { AccountGroup, AccountRow, AccountsBundle, AccountsPageParams } from '../model/types';

const accountArchiveTimeoutMs = 30 * 60 * 1_000;

const accountSchema = z.object({
  id: z.string(),
  label: z.string().optional(),
  email: z.string().optional(),
  provider: z.string().optional(),
  status: z.string().optional(),
  group_name: z.string().optional(),
  plan_type: z.string().optional(),
  auth_method: z.string().optional(),
  billing_mode: z.string().optional(),
  api_key_present: z.boolean().optional(),
  ignore_rate_limit_controls: z.boolean().optional(),
  quarantine_until: z.number().optional(),
  quarantine_reason: z.string().optional(),
  capabilities: z.array(z.object({
    model_slug: z.string().optional(),
    availability_state: z.string().optional(),
    context_1m_state: z.string().optional(),
    context_1m_source: z.string().optional(),
    native_context_window: z.coerce.number().optional(),
    native_max_context_window: z.coerce.number().optional(),
    source: z.string().optional(),
  }).passthrough()).optional(),
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

export interface AccountArchiveDownload {
  blob: Blob;
  filename: string;
}

export interface AccountArchiveImportResult {
  recognized: number;
  imported: number;
  replaced: number;
  files: number;
  zip: boolean;
  formats: string[];
  accounts: Array<{ id: string; label?: string; provider?: string; status: string }>;
}

function responseHeader(headers: unknown, name: string): string {
  if (!headers || typeof headers !== 'object') return '';
  const candidate = headers as Record<string, unknown> & { get?: (headerName: string) => unknown };
  if (typeof candidate.get === 'function') {
    const value = candidate.get(name);
    if (value != null) return String(value);
  }
  const value = candidate[name] ?? candidate[name.toLowerCase()];
  return value == null ? '' : String(value);
}

function filenameFromDisposition(value: unknown): string {
  const raw = String(value || '');
  const utf8 = raw.match(/filename\*=UTF-8''([^;]+)/i);
  if (utf8?.[1]) {
    const encoded = utf8[1].trim().replace(/^"|"$/g, '');
    try {
      return decodeURIComponent(encoded);
    } catch {
      return encoded;
    }
  }
  return raw.match(/filename=([^;]+)/i)?.[1]?.trim().replace(/^"|"$/g, '') || '';
}

function archiveBlob(data: unknown, contentType: string): Blob {
  return data instanceof Blob ? data : new Blob([data as BlobPart], { type: contentType });
}

export async function fetchAccountArchive(ids: string[] = [], signal?: AbortSignal): Promise<AccountArchiveDownload> {
  const normalizedIDs = [...new Set(ids.map((id) => String(id || '').trim()).filter(Boolean))];
  const response = await api.get('/admin/accounts/export', {
    params: {
      format: 'backup',
      ...(normalizedIDs.length ? { ids: normalizedIDs.join(',') } : {}),
    },
    responseType: 'blob',
    timeout: accountArchiveTimeoutMs,
    ...(signal ? { signal } : {}),
  });
  const contentType = responseHeader(response.headers, 'content-type').toLowerCase();
  const isZIP = contentType.includes('application/zip');
  const isJSON = contentType.includes('application/json');
  if (!isZIP && !isJSON) throw new Error('服务器未返回账号 JSON 或 ZIP 备份。');
  const blob = archiveBlob(response.data, isZIP ? 'application/zip' : 'application/json');
  if (blob.size < 2) throw new Error('账号备份文件为空或不完整。');
  const prefix = new Uint8Array(await blob.slice(0, Math.min(32, blob.size)).arrayBuffer());
  if (isZIP && (prefix[0] !== 0x50 || prefix[1] !== 0x4b)) {
    throw new Error('服务器返回的账号 ZIP 备份无效。');
  }
  if (isJSON) {
    const text = new TextDecoder().decode(prefix).trimStart();
    if (!text.startsWith('{') && !text.startsWith('[')) {
      throw new Error('服务器返回的账号 JSON 备份无效。');
    }
  }
  const fallbackName = isZIP ? 'account-pool.zip' : 'account.json';
  const filename = filenameFromDisposition(responseHeader(response.headers, 'content-disposition')) || fallbackName;
  return { blob, filename };
}

export async function importAccountArchive(file: File, signal?: AbortSignal): Promise<AccountArchiveImportResult> {
  if (!(file instanceof File) || file.size === 0) throw new Error('请选择非空的 JSON 或 ZIP 文件。');
  const form = new FormData();
  form.append('file', file, file.name);
  const response = await api.post('/admin/accounts/import-archive', form, {
    timeout: accountArchiveTimeoutMs,
    ...(signal ? { signal } : {}),
  });
  const data = response.data as Partial<AccountArchiveImportResult>;
  if (!Number.isFinite(data.recognized) || Number(data.recognized) <= 0 || !Array.isArray(data.accounts)) {
    throw new Error('服务器返回了无效的账号导入结果。');
  }
  return {
    recognized: Number(data.recognized),
    imported: Number(data.imported || 0),
    replaced: Number(data.replaced || 0),
    files: Number(data.files || 0),
    zip: Boolean(data.zip),
    formats: Array.isArray(data.formats) ? data.formats.map(String) : [],
    accounts: data.accounts,
  };
}
