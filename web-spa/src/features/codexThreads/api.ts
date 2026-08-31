import { get, getToken, post } from '../../api.js';
import type {
  CodexRuntime,
  CodexThread,
  CodexThreadFilters,
  CodexThreadPage,
  CodexThreadStatusEvent,
} from './types';

type JsonRecord = Record<string, unknown>;

function recordOf(value: unknown): JsonRecord | null {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as JsonRecord : null;
}

function rowsOf(value: unknown, key = 'data'): JsonRecord[] {
  const record = recordOf(value);
  const rows = record?.[key];
  return Array.isArray(rows) ? rows.map(recordOf).filter((row): row is JsonRecord => row !== null) : [];
}

function text(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function flag(value: unknown): boolean {
  return value === true;
}

function count(value: unknown): number {
  const number = Number(value);
  return Number.isSafeInteger(number) && number >= 0 ? number : 0;
}

function threadOf(value: JsonRecord): CodexThread {
  return {
    runtimeId: text(value.runtime_id),
    runtimeLabel: text(value.runtime_label) || undefined,
    threadKey: text(value.thread_key),
    threadHandle: text(value.thread_handle),
    model: text(value.model) || undefined,
    modelProvider: text(value.model_provider) || undefined,
    source: text(value.source) || undefined,
    status: text(value.status) as CodexThread['status'] || 'unknown',
    waitingReason: text(value.waiting_reason) as CodexThread['waitingReason'],
    activeTurnHandle: text(value.active_turn_handle) || undefined,
    runtimeAvailable: flag(value.runtime_available),
    updatedAt: text(value.updated_at) || undefined,
    directInput: flag(value.direct_input),
    cwdBase: text(value.cwd_basename) || undefined,
    revision: count(value.revision),
  };
}

function runtimeOf(value: JsonRecord): CodexRuntime {
  return {
    id: text(value.id),
    label: text(value.label) || undefined,
    generation: count(value.generation),
    available: flag(value.available),
    lastHeartbeat: text(value.last_heartbeat) || undefined,
  };
}

function filterParams(filters: CodexThreadFilters, cursor?: string): Record<string, string | undefined> {
  return {
    runtime_id: filters.runtimeId,
    cursor,
    sort_key: filters.sortKey,
    sort_direction: filters.sortDirection,
    model_providers: filters.modelProviders?.join(','),
    source_kinds: filters.sourceKinds?.join(','),
    archived: filters.archived === undefined ? undefined : String(filters.archived),
    is_pinned: filters.isPinned === undefined ? undefined : String(filters.isPinned),
    search_term: filters.searchTerm?.trim() || undefined,
  };
}

export async function fetchCodexRuntimes(signal?: AbortSignal): Promise<CodexRuntime[]> {
  const payload = await get('/admin/codex-runtimes', undefined, { signal });
  return rowsOf(payload).map(runtimeOf).filter((runtime) => runtime.id !== '');
}

export async function fetchCodexThreads(filters: CodexThreadFilters, cursor?: string, signal?: AbortSignal): Promise<CodexThreadPage> {
  if (!filters.runtimeId) return { data: [] };
  const payload = await get('/admin/codex-threads', filterParams(filters, cursor), { signal });
  const record = recordOf(payload);
  return {
    data: rowsOf(payload).map(threadOf).filter((thread) => thread.threadKey !== '' && thread.threadHandle !== ''),
    nextCursor: text(record?.next_cursor) || undefined,
    backwardsCursor: text(record?.backwards_cursor) || undefined,
  };
}

export async function resumeCodexThread(threadHandle: string, signal?: AbortSignal): Promise<CodexThread> {
  const payload = await post(`/admin/codex-threads/${encodeURIComponent(threadHandle)}/resume`, {}, { signal });
  const value = recordOf(payload);
  if (!value) throw new Error('Codex thread resume response is invalid');
  return threadOf(value);
}

export async function interruptCodexThread(threadHandle: string, turnHandle: string, signal?: AbortSignal): Promise<void> {
  await post(
    `/admin/codex-threads/${encodeURIComponent(threadHandle)}/turns/${encodeURIComponent(turnHandle)}/interrupt`,
    {},
    { signal },
  );
}

export function codexThreadErrorCode(error: unknown): string {
  const candidate = error as { code?: unknown; response?: { data?: { error?: { code?: unknown } } } };
  if (typeof candidate?.response?.data?.error?.code === 'string') return candidate.response.data.error.code;
  return typeof candidate?.code === 'string' ? candidate.code : '';
}

function eventError(status: number, body: unknown): Error {
  const code = recordOf(recordOf(body)?.error)?.code;
  const error = new Error(typeof code === 'string' ? code : `Codex event stream failed (${status})`) as Error & { code?: string; status?: number };
  error.code = typeof code === 'string' ? code : undefined;
  error.status = status;
  return error;
}

export interface CodexThreadEventSubscription {
  runtimeId: string;
  signal: AbortSignal;
  onStatus: (event: CodexThreadStatusEvent) => void;
}

// fetch-based SSE keeps browser cookie authentication and legacy Bearer admin
// tokens on the same path. EventSource cannot attach the latter's Authorization
// header, which would silently disable live status for those administrators.
export async function subscribeCodexThreadEvents(subscription: CodexThreadEventSubscription): Promise<void> {
  const params = new URLSearchParams({ runtime_id: subscription.runtimeId });
  const headers = new Headers({ Accept: 'text/event-stream' });
  const token = getToken();
  if (token) headers.set('Authorization', `Bearer ${token}`);
  const response = await fetch(`/admin/codex-threads/events?${params.toString()}`, {
    method: 'GET', headers, credentials: 'include', signal: subscription.signal,
  });
  if (!response.ok) {
    let body: unknown = null;
    try { body = await response.json(); } catch { /* retain a safe generic error */ }
    throw eventError(response.status, body);
  }
  if (!response.body) throw new Error('Codex event stream is unavailable');

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffered = '';
  let name = '';
  let data: string[] = [];
  const dispatch = () => {
    if (name !== 'thread/status/changed' || !data.length) return;
    try {
      const value = recordOf(JSON.parse(data.join('\n')));
      if (!value) return;
      const event = threadOf(value);
      if (event.threadKey && event.threadHandle) subscription.onStatus(event);
    } catch {
      // A malformed notification is never allowed to take down the page or
      // mutate an unrelated row. The next monotonic event can still recover it.
    }
  };
  try {
    while (true) {
      const chunk = await reader.read();
      if (chunk.done) break;
      buffered += decoder.decode(chunk.value, { stream: true });
      let newline = buffered.indexOf('\n');
      while (newline >= 0) {
        const line = buffered.slice(0, newline).replace(/\r$/, '');
        buffered = buffered.slice(newline + 1);
        if (line === '') {
          dispatch();
          name = '';
          data = [];
        } else if (line.startsWith('event:')) {
          name = line.slice('event:'.length).trim();
        } else if (line.startsWith('data:')) {
          data.push(line.slice('data:'.length).trimStart());
        }
        newline = buffered.indexOf('\n');
      }
    }
  } finally {
    reader.releaseLock();
  }
}
