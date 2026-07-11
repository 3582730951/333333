import axios from 'axios';
import { createApiError } from './contracts';
import type { ApiError } from '../model/contracts';

function bodyMessage(data: unknown): string {
  if (!data || typeof data !== 'object') return '';
  const value = data as Record<string, unknown>;
  if (typeof value.message === 'string') return value.message;
  if (typeof value.error === 'string') return value.error;
  if (value.error && typeof value.error === 'object' && typeof (value.error as Record<string, unknown>).message === 'string') {
    return String((value.error as Record<string, unknown>).message);
  }
  return '';
}

function bodyRequestId(data: unknown): string {
  if (!data || typeof data !== 'object') return '';
  const value = data as Record<string, unknown>;
  if (typeof value.request_id === 'string') return value.request_id;
  if (value.error && typeof value.error === 'object' && typeof (value.error as Record<string, unknown>).request_id === 'string') {
    return String((value.error as Record<string, unknown>).request_id);
  }
  return '';
}

export function normalizeApiError(error: unknown): ApiError {
  if (error && typeof error === 'object' && (error as { name?: string }).name === 'ApiError') return error as ApiError;
  if (axios.isCancel(error) || (error instanceof DOMException && error.name === 'AbortError')) {
    return createApiError({ code: 'REQUEST_ABORTED', userMessage: '请求已取消', cause: error });
  }

  if (axios.isAxiosError(error)) {
    const status = error.response?.status ?? 0;
    const data = error.response?.data;
    const requestId = bodyRequestId(data) || String(error.response?.headers?.['x-request-id'] ?? '');
    const offline = !error.response;
    const fallback = status === 401
      ? '登录已失效，请重新登录。'
      : status === 403
        ? '当前账号没有执行此操作的权限。'
        : status >= 500
          ? '服务暂时不可用，请稍后重试。'
          : offline
            ? '无法连接服务器，请检查网络后重试。'
            : '请求失败，请重试。';
    return createApiError({
      status,
      code: String((data as Record<string, any>)?.error?.code ?? (data as Record<string, any>)?.code ?? error.code ?? 'REQUEST_FAILED'),
      requestId,
      retryable: offline || status === 408 || status === 429 || status >= 500,
      userMessage: bodyMessage(data) || fallback,
      cause: error,
    });
  }

  const message = error instanceof Error ? error.message : String(error || '未知错误');
  return createApiError({ code: 'CLIENT_ERROR', userMessage: message, cause: error });
}
