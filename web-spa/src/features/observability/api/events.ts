import { z } from 'zod';
import { get } from '../../../api.js';
import { parseApiResponse } from '../../../api/contracts';
import type { AuditRow, CFEventRow, QuotaRow } from '../model/types';

const quotaRowSchema = z.object({ account_id: z.string() }).passthrough();
const cfEventSchema = z.object({
  id: z.union([z.string(), z.number()]).optional(),
  created_at: z.number().optional(),
  status: z.union([z.string(), z.number()]).optional(),
  message: z.string().optional(),
}).passthrough();
const auditRowSchema = z.object({
  id: z.union([z.string(), z.number()]).optional(),
  created_at: z.number().optional(),
  action: z.string().optional(),
}).passthrough();

export function rowsResponseSchema<T extends z.ZodTypeAny>(row: T, keys: string[]) {
  return z.union([
    z.array(row),
    z.record(z.string(), z.unknown()).transform((value) => {
      for (const key of keys) if (Array.isArray(value[key])) return value[key];
      return [];
    }).pipe(z.array(row)),
  ]);
}

export const quotaResponseSchema = rowsResponseSchema(quotaRowSchema, ['quota', 'rows']);
export const cfEventsResponseSchema = rowsResponseSchema(cfEventSchema, ['events', 'rows']);
export const auditResponseSchema = rowsResponseSchema(auditRowSchema, ['rows', 'events']);

export async function fetchQuota(signal?: AbortSignal): Promise<QuotaRow[]> {
  const value = await get('/admin/quota', { include_missing: 1, page: 1, pageSize: 500 }, { signal });
  return parseApiResponse(quotaResponseSchema, value) as QuotaRow[];
}

export async function fetchCFEvents(signal?: AbortSignal): Promise<CFEventRow[]> {
  const value = await get('/admin/cf-events', { limit: 300 }, { signal });
  return parseApiResponse(cfEventsResponseSchema, value) as CFEventRow[];
}

export async function fetchAuditRows(signal?: AbortSignal): Promise<AuditRow[]> {
  const value = await get('/admin/audit', { limit: 500 }, { signal });
  return parseApiResponse(auditResponseSchema, value) as AuditRow[];
}
