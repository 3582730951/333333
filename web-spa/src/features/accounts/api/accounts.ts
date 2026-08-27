import { z } from 'zod';
import api from '../../../api.js';
import { get } from '../../../api.js';
import { createApiError, parseApiResponse } from '../../../api/contracts';
import type { AccountGroup, AccountRow, AccountsBundle, AccountsPageParams } from '../model/types';

const accountArchiveTimeoutMs = 30 * 60 * 1_000;

const optionalString = z.preprocess(
  (value) => value == null ? undefined : value,
  z.string().optional(),
);
const optionalBoolean = z.preprocess((value) => {
  if (value == null || value === '') return undefined;
  if (value === 1 || value === '1' || value === 'true') return true;
  if (value === 0 || value === '0' || value === 'false') return false;
  return value;
}, z.boolean().optional());
const optionalNumber = z.preprocess(
  (value) => value == null || value === '' ? undefined : value,
  z.coerce.number().finite().optional(),
);

const accountSchema = z.object({
  id: z.preprocess((value) => typeof value === 'number' ? String(value) : value, z.string().min(1)),
  label: optionalString,
  email: optionalString,
  provider: optionalString,
  status: optionalString,
  group_name: optionalString,
  plan_type: optionalString,
  auth_method: optionalString,
  credential_mode: optionalString,
  billing_mode: optionalString,
  api_key_present: optionalBoolean,
  ignore_rate_limit_controls: optionalBoolean,
  force_codex_429: optionalBoolean,
	 routing_weight: optionalNumber,
	 retry_max_attempts: optionalNumber,
  quarantine_until: optionalNumber,
  quarantine_reason: optionalString,
  capabilities: z.array(z.object({
    model_slug: z.string().optional(),
    availability_state: z.string().optional(),
    context_1m_state: z.string().optional(),
    context_1m_source: z.string().optional(),
    native_context_window: z.coerce.number().optional(),
    native_max_context_window: z.coerce.number().optional(),
    source: z.string().optional(),
  }).passthrough()).nullable().optional().transform((value) => value ?? []),
  usage: z.record(z.string(), z.unknown()).nullable().optional(),
}).passthrough();

const accountPageObjectSchema = z.object({
  accounts: z.array(accountSchema).optional(),
  rows: z.array(accountSchema).optional(),
  items: z.array(accountSchema).optional(),
  data: z.array(accountSchema).optional(),
  total: z.coerce.number().int().nonnegative().optional(),
}).passthrough().refine(
  (value) => [value.accounts, value.rows, value.items, value.data].some(Array.isArray),
  { message: 'account collection is missing' },
).transform((value) => {
  const rows = value.accounts ?? value.rows ?? value.items ?? value.data ?? [];
  return { rows, total: value.total ?? rows.length };
});

export const accountsResponseSchema = z.union([
  z.array(accountSchema).transform((rows) => ({ rows, total: rows.length })),
  accountPageObjectSchema,
  z.object({
    data: z.union([
      z.array(accountSchema).transform((rows) => ({ rows, total: rows.length })),
      accountPageObjectSchema,
    ]),
  }).passthrough().transform((value) => value.data),
]);

function firstContractIssue(cause: unknown): { path: Array<string | number>; message: string } | null {
  if (!cause || typeof cause !== 'object') return null;
  const issues = (cause as { issues?: unknown[] }).issues;
  if (!Array.isArray(issues)) return null;
  const queue = [...issues] as Array<Record<string, unknown>>;
  while (queue.length) {
    const issue = queue.shift() || {};
    const nested = issue.unionErrors;
    if (Array.isArray(nested)) {
      nested.forEach((error) => {
        const childIssues = error && typeof error === 'object' ? (error as { issues?: unknown[] }).issues : null;
        if (Array.isArray(childIssues)) queue.push(...childIssues as Array<Record<string, unknown>>);
      });
    }
    if (Array.isArray(issue.path) && issue.path.length) {
      return { path: issue.path as Array<string | number>, message: String(issue.message || '') };
    }
  }
  return null;
}

function accountContractMessage(payload: unknown, cause: unknown): string {
  if (typeof payload === 'string') {
    const looksLikeHTML = /^\s*<!doctype|^\s*<html/i.test(payload);
    return looksLikeHTML
      ? '账号接口返回了前端页面而不是 JSON。请检查反向代理是否把 /admin/accounts 回退到了网页入口。'
      : '账号接口返回了文本而不是账号 JSON。请检查网关的内容类型和 API 路由。';
  }
  if (payload && typeof payload === 'object' && !Array.isArray(payload) && 'error' in payload) {
    return '账号接口以成功状态返回了错误对象。请检查网关状态码透传和登录会话。';
  }
  const issue = firstContractIssue(cause);
  if (issue) {
    const path = issue.path.map((part) => typeof part === 'number' ? `第 ${part + 1} 项` : part).join(' / ');
    return `账号响应字段“${path}”的类型与当前版本不兼容；已记录本次请求信息。`;
  }
  return '账号接口响应结构与当前版本不兼容；已记录本次请求信息。';
}

const groupSchema = z.object({ name: z.string() }).passthrough();
export const accountGroupsResponseSchema = z.union([
  z.array(groupSchema),
  z.object({ groups: z.array(groupSchema).optional() }).passthrough().transform((value) => value.groups ?? []),
]);

export async function fetchAccountsPage(params: AccountsPageParams, signal?: AbortSignal) {
  const response = await api.get('/admin/accounts', { params, signal });
  const requestID = responseHeader(response.headers, 'x-request-id');
  try {
    return parseApiResponse(accountsResponseSchema, response.data, requestID) as { rows: AccountRow[]; total: number };
  } catch (error) {
    if (error && typeof error === 'object' && (error as { code?: string }).code === 'INVALID_RESPONSE') {
      throw createApiError({
        status: 502,
        code: 'ACCOUNTS_RESPONSE_INCOMPATIBLE',
        requestId: requestID,
        retryable: false,
        userMessage: accountContractMessage(response.data, (error as { cause?: unknown }).cause),
        cause: (error as { cause?: unknown }).cause,
      });
    }
    throw error;
  }
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
  exported?: number;
  skipped?: number;
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

export type AccountExportFormat = 'backup' | 'cliproxyapi' | 'codex-auth';

export async function fetchAccountArchive(ids: string[] = [], formatOrSignal: AccountExportFormat | AbortSignal = 'backup', signal?: AbortSignal): Promise<AccountArchiveDownload> {
  const format: AccountExportFormat = typeof formatOrSignal === 'string' ? formatOrSignal : 'backup';
  const requestSignal = typeof formatOrSignal === 'string' ? signal : formatOrSignal;
  const normalizedIDs = [...new Set(ids.map((id) => String(id || '').trim()).filter(Boolean))];
  const response = await api.get('/admin/accounts/export', {
    params: {
      format,
      ...(normalizedIDs.length ? { ids: normalizedIDs.join(',') } : {}),
    },
    responseType: 'blob',
    timeout: accountArchiveTimeoutMs,
    ...(requestSignal ? { signal: requestSignal } : {}),
  });
  const contentType = responseHeader(response.headers, 'content-type').toLowerCase();
  const isZIP = contentType.includes('application/zip');
  const isJSON = contentType.includes('application/json');
  if (!isZIP && !isJSON) throw new Error('服务器未返回账号 JSON 或 ZIP 导出文件。');
  const blob = archiveBlob(response.data, isZIP ? 'application/zip' : 'application/json');
  if (blob.size < 2) throw new Error('账号导出文件为空或不完整。');
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
  const exported = Number(responseHeader(response.headers, 'x-accounts-exported'));
  const skipped = Number(responseHeader(response.headers, 'x-accounts-skipped'));
  return {
    blob,
    filename,
    ...(Number.isFinite(exported) && exported > 0 ? { exported } : {}),
    ...(Number.isFinite(skipped) && skipped > 0 ? { skipped } : {}),
  };
}

export async function importAccountArchive(file: File, signal?: AbortSignal): Promise<AccountArchiveImportResult> {
  if (!(file instanceof File) || file.size === 0) throw new Error('请选择非空的 JSON 或 ZIP 文件。');
  const form = new FormData();
  form.append('file', file, file.name);
  const response = await api.post('/admin/accounts/import-archive', form, {
    timeout: accountArchiveTimeoutMs,
    ...(signal ? { signal } : {}),
  });
  const responseData = response.data as unknown;
  const responseRecord = responseData && typeof responseData === 'object' ? responseData as Record<string, unknown> : {};
  const data = (responseRecord.data && typeof responseRecord.data === 'object'
    ? responseRecord.data
    : responseRecord) as Partial<AccountArchiveImportResult>;
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
