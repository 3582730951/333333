import { z } from 'zod';
import type { ApiError, PageResult } from '../model/contracts';

const unknownRecord = z.record(z.string(), z.unknown());

export const apiEnvelopeSchema = z.object({
  data: z.unknown().optional(),
  request_id: z.string().optional(),
}).passthrough();

export function pageResultSchema<T extends z.ZodTypeAny>(rowSchema: T) {
  return z.union([
    z.array(rowSchema).transform((rows): PageResult<z.infer<T>> => ({ rows, total: rows.length, page: 1, pageSize: rows.length })),
    z.object({
      rows: z.array(rowSchema).optional(),
      items: z.array(rowSchema).optional(),
      data: z.array(rowSchema).optional(),
      total: z.coerce.number().int().nonnegative().optional(),
      page: z.coerce.number().int().positive().optional(),
      page_size: z.coerce.number().int().nonnegative().optional(),
      pageSize: z.coerce.number().int().nonnegative().optional(),
    }).passthrough().transform((value): PageResult<z.infer<T>> => {
      const rows = value.rows ?? value.items ?? value.data ?? [];
      return {
        rows,
        total: value.total ?? rows.length,
        page: value.page ?? 1,
        pageSize: value.page_size ?? value.pageSize ?? rows.length,
      };
    }),
  ]);
}

export const looseRowSchema = unknownRecord;

export function createApiError(input: Partial<ApiError> & Pick<ApiError, 'userMessage'>): ApiError {
  const error = new Error(input.userMessage) as ApiError;
  error.name = 'ApiError';
  error.status = input.status ?? 0;
  error.code = input.code ?? 'UNKNOWN_ERROR';
  error.requestId = input.requestId ?? '';
  error.retryable = input.retryable ?? false;
  error.userMessage = input.userMessage;
  error.cause = input.cause;
  return error;
}

export function parseApiResponse<T>(schema: z.ZodType<T>, value: unknown, requestId = ''): T {
  const result = schema.safeParse(value);
  if (result.success) return result.data;
  throw createApiError({
    status: 502,
    code: 'INVALID_RESPONSE',
    requestId,
    retryable: false,
    userMessage: '接口返回了无法识别的数据，请联系管理员并附上请求 ID。',
    cause: result.error,
  });
}
