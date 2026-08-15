import api from '../../../api.js';

export type AuditExportKind = 'diagnostics' | 'cache-hits';

export interface AuditArchive {
  blob: Blob;
  filename: string;
}

export interface DiagnosticExportOptions {
  pollIntervalMs?: number;
  timeoutMs?: number;
  queueTimeoutMs?: number;
  queuedStallMs?: number;
  wait?: (milliseconds: number) => Promise<void>;
  now?: () => number;
  signal?: AbortSignal;
}

interface DiagnosticJob {
  id: string;
  status: string;
  download_url?: string;
  error_code?: string;
}

const archiveDefinitions: Record<Exclude<AuditExportKind, 'diagnostics'>, { endpoint: string; fallbackName: string }> = {
  'cache-hits': { endpoint: '/admin/export/cache-hits', fallbackName: 'codex-pool-cache-hits.zip' },
};

const diagnosticFallbackName = 'codex-pool-diagnostics.zip';
const diagnosticPollIntervalMs = 2_000;
const diagnosticTimeoutMs = 5 * 60 * 1_000;
const diagnosticQueueTimeoutMs = 10 * 60 * 1_000;
const diagnosticQueuedStallMs = 15_000;
const pendingDiagnosticStatuses = new Set(['queued', 'snapshotting', 'rendering', 'validating']);
const runningDiagnosticStatuses = new Set(['snapshotting', 'rendering', 'validating']);
const terminalDiagnosticStatuses = new Set(['failed', 'cancelled', 'expired']);

export function filenameFromDisposition(value: unknown): string {
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
  const plain = raw.match(/filename=([^;]+)/i);
  return plain?.[1]?.trim().replace(/^"|"$/g, '') || '';
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

function diagnosticJobFromResponse(value: unknown): DiagnosticJob {
  if (!value || typeof value !== 'object') throw new Error('Invalid diagnostic job response.');
  const envelope = value as { job?: unknown };
  const raw = envelope.job && typeof envelope.job === 'object' ? envelope.job : value;
  const job = raw as Partial<DiagnosticJob>;
  const id = String(job.id || '').trim();
  const status = String(job.status || '').trim().toLowerCase();
  if (!/^diagjob_[A-Za-z0-9_-]+$/.test(id) || !status) {
    throw new Error('Invalid diagnostic job response.');
  }
  return {
    id,
    status,
    download_url: typeof job.download_url === 'string' ? job.download_url : undefined,
    error_code: typeof job.error_code === 'string' ? job.error_code : undefined,
  };
}

function diagnosticJobsFromListResponse(value: unknown): DiagnosticJob[] {
  if (!value || typeof value !== 'object') return [];
  const jobs = (value as { jobs?: unknown }).jobs;
  if (!Array.isArray(jobs)) return [];
  const parsed: DiagnosticJob[] = [];
  for (const candidate of jobs) {
    try {
      parsed.push(diagnosticJobFromResponse(candidate));
    } catch {
      // A malformed unrelated historical row must not strand the current export.
    }
  }
  return parsed;
}

function diagnosticAbortError(): DOMException {
  return new DOMException('Diagnostic export cancelled.', 'AbortError');
}

function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw diagnosticAbortError();
}

async function waitWithAbort(
  wait: ((milliseconds: number) => Promise<void>) | undefined,
  milliseconds: number,
  signal?: AbortSignal,
): Promise<void> {
  if (!wait) {
    if (!signal) {
      await new Promise<void>((resolve) => {
        window.setTimeout(resolve, milliseconds);
      });
      return;
    }
    throwIfAborted(signal);
    await new Promise<void>((resolve, reject) => {
      let timer: number | undefined;
      let settled = false;
      const finish = (succeeded: boolean, error?: unknown) => {
        if (settled) return;
        settled = true;
        if (timer !== undefined) window.clearTimeout(timer);
        signal.removeEventListener('abort', onAbort);
        if (succeeded) resolve();
        else reject(error);
      };
      const onAbort = () => finish(false, diagnosticAbortError());
      signal.addEventListener('abort', onAbort, { once: true });
      // AbortSignal does not replay an event to listeners registered after the
      // transition. Recheck after registration to close that race.
      if (signal.aborted) {
        onAbort();
        return;
      }
      timer = window.setTimeout(() => finish(true), milliseconds);
    });
    return;
  }
  if (!signal) {
    await wait(milliseconds);
    return;
  }
  throwIfAborted(signal);
  await new Promise<void>((resolve, reject) => {
    let settled = false;
    const finish = (succeeded: boolean, error?: unknown) => {
      if (settled) return;
      settled = true;
      signal.removeEventListener('abort', onAbort);
      if (succeeded) resolve();
      else reject(error);
    };
    const onAbort = () => finish(false, diagnosticAbortError());
    signal.addEventListener('abort', onAbort, { once: true });
    if (signal.aborted) {
      onAbort();
      return;
    }
    Promise.resolve().then(() => wait(milliseconds)).then(
      () => finish(true),
      (error) => finish(false, error),
    );
  });
}

function diagnosticDownloadURL(job: DiagnosticJob, jobPath: string): string {
  const expectedPath = `${jobPath}/download`;
  const candidate = String(job.download_url || '').trim();
  if (!candidate) return expectedPath;
  const parsed = new URL(candidate, window.location.href);
  if (
    parsed.origin !== window.location.origin ||
    parsed.username ||
    parsed.password ||
    parsed.pathname !== expectedPath ||
    parsed.hash
  ) {
    throw new Error('The server returned an invalid diagnostic download URL.');
  }
  return `${parsed.pathname}${parsed.search}`;
}

async function zipBlobFromResponse(response: {
  data: unknown;
  headers?: unknown;
}, fallbackName: string): Promise<AuditArchive> {
  const contentType = responseHeader(response.headers, 'content-type').toLowerCase();
  if (!contentType.includes('application/zip') && !contentType.includes('application/octet-stream')) {
    throw new Error('The server did not return a diagnostic ZIP archive.');
  }
  const blob = response.data instanceof Blob
    ? response.data
    : new Blob([response.data as BlobPart], { type: 'application/zip' });
  if (blob.size < 4) throw new Error('The diagnostic ZIP archive is incomplete.');
  const signature = new Uint8Array(await blob.slice(0, 4).arrayBuffer());
  if (signature[0] !== 0x50 || signature[1] !== 0x4b) {
    throw new Error('The server returned an invalid diagnostic ZIP archive.');
  }
  const filename = filenameFromDisposition(responseHeader(response.headers, 'content-disposition')) || fallbackName;
  return { blob, filename };
}

async function fetchDirectArchive(
  endpoint: string,
  fallbackName: string,
  signal?: AbortSignal,
  timeout?: number,
): Promise<AuditArchive> {
  // Let the browser construct the Blob directly. Fetching an ArrayBuffer and then
  // wrapping it in a Blob can briefly retain two full copies of a large archive.
  throwIfAborted(signal);
  const response = await api.get(endpoint, {
    responseType: 'blob',
    ...(timeout ? { timeout } : {}),
    ...(signal ? { signal } : {}),
  });
  const archive = await zipBlobFromResponse(response, fallbackName);
  throwIfAborted(signal);
  return archive;
}

async function fetchDiagnosticArchive(options: DiagnosticExportOptions = {}): Promise<AuditArchive> {
  const pollIntervalMs = options.pollIntervalMs ?? diagnosticPollIntervalMs;
  const timeoutMs = options.timeoutMs ?? diagnosticTimeoutMs;
  const queueTimeoutMs = options.queueTimeoutMs ?? diagnosticQueueTimeoutMs;
  const queuedStallMs = options.queuedStallMs ?? diagnosticQueuedStallMs;
  const wait = options.wait;
  const now = options.now ?? Date.now;
  const signal = options.signal;
  throwIfAborted(signal);
  // Do not abort the create request. If the server commits the job just before a
  // browser abort, cancelling this POST would hide the assigned id and leave an
  // orphan renderer. Wait for the tiny JSON response, then the guarded block
  // below observes the abort and sends a detached DELETE using the known id.
  const created = await api.post('/admin/diagnostics/jobs', {});
  let job = diagnosticJobFromResponse(created.data);
  const jobPath = `/admin/diagnostics/jobs/${encodeURIComponent(job.id)}`;
  const startedAt = now();
  const queueDeadline = startedAt + queueTimeoutMs;
  let runningDeadline = job.status === 'queued' ? 0 : startedAt + timeoutMs;
  let queuedSince = job.status === 'queued' ? startedAt : 0;
  let cancelRequested = false;

  const cancelJob = async () => {
    if (cancelRequested) return;
    cancelRequested = true;
    // Deliberately do not reuse the aborted request signal: the server still
    // needs this best-effort cancellation after navigation/unmount.
    await Promise.resolve().then(() => api.delete(jobPath)).catch(() => undefined);
  };

  try {
    while (job.status !== 'ready') {
      throwIfAborted(signal);
      if (terminalDiagnosticStatuses.has(job.status)) {
        const suffix = job.error_code ? ` (${job.error_code})` : '';
        throw new Error(`Diagnostic export ${job.status}${suffix}.`);
      }
      if (!pendingDiagnosticStatuses.has(job.status)) {
        throw new Error('The server returned an unknown diagnostic job status.');
      }
      const checkedAt = now();
      if (job.status === 'queued') {
        if (checkedAt >= queueDeadline) {
          throw new Error('Diagnostic export queue timed out.');
        }
        if (!queuedSince) queuedSince = checkedAt;
        if (checkedAt - queuedSince >= queuedStallMs) {
          const listResponse = await (signal
            ? api.get('/admin/diagnostics/jobs', { signal })
            : api.get('/admin/diagnostics/jobs')).catch(() => null);
          throwIfAborted(signal);
          const queuedJobs = diagnosticJobsFromListResponse(listResponse?.data);
          const current = queuedJobs.find((candidate) => candidate.id === job.id);
          if (current && current.status !== 'queued') {
            job = current;
            runningDeadline = checkedAt + timeoutMs;
            queuedSince = 0;
            continue;
          }
          const anotherJobIsRunning = queuedJobs.some((candidate) => (
            candidate.id !== job.id && runningDiagnosticStatuses.has(candidate.status)
          ));
          if (anotherJobIsRunning || listResponse === null) {
            // This job is legitimately behind the single global renderer, or queue
            // health could not be confirmed. The overall deadline remains enforced.
            queuedSince = checkedAt;
          } else {
            throw new Error('Diagnostic export worker did not claim the queued job before the queue-stall deadline.');
          }
        }
      } else {
        queuedSince = 0;
        if (!runningDeadline) runningDeadline = checkedAt + timeoutMs;
      }
      if (runningDeadline && checkedAt >= runningDeadline) {
        throw new Error('Diagnostic export timed out.');
      }
      if (pollIntervalMs > 0) {
        await waitWithAbort(wait, pollIntervalMs, signal);
      }
      const statusResponse = signal
        ? await api.get(jobPath, { signal })
        : await api.get(jobPath);
      job = diagnosticJobFromResponse(statusResponse.data);
      if (job.id !== jobPath.slice(jobPath.lastIndexOf('/') + 1)) {
        throw new Error('The diagnostic job identity changed while polling.');
      }
    }

    throwIfAborted(signal);
    const downloadURL = diagnosticDownloadURL(job, jobPath);
    const response = await api.get(downloadURL, {
      responseType: 'blob',
      // The shared Axios client has a 30-second default intended for JSON APIs.
      // A validated multi-megabyte support archive may legitimately take longer
      // to transfer even though rendering is already complete.
      timeout: Math.max(timeoutMs, 60_000),
      ...(signal ? { signal } : {}),
    });
    const archive = await zipBlobFromResponse(response, diagnosticFallbackName);
    throwIfAborted(signal);
    cancelRequested = true;
    return archive;
  } catch (error) {
    await cancelJob();
    throw error;
  }
}

export async function fetchAuditArchive(
  kind: AuditExportKind,
  diagnosticOptions: DiagnosticExportOptions = {},
): Promise<AuditArchive> {
  if (kind === 'diagnostics') {
    try {
      return await fetchDiagnosticArchive(diagnosticOptions);
    } catch (primaryError) {
      // A context-loss incident can coincide with a stopped or stranded optional
      // diagnostics worker. Preserve cancellation semantics, but otherwise fall
      // back to the authenticated same-origin rescue stream on the request worker.
      // This path uses the existing admin export route and still validates content
      // type plus the ZIP signature before exposing a download.
      throwIfAborted(diagnosticOptions.signal);
      try {
        return await fetchDirectArchive(
          '/admin/export/logs?mode=rescue',
          diagnosticFallbackName,
          diagnosticOptions.signal,
          Math.max(diagnosticOptions.timeoutMs ?? diagnosticTimeoutMs, 60_000),
        );
      } catch (rescueError) {
        throwIfAborted(diagnosticOptions.signal);
        const primary = primaryError instanceof Error ? primaryError.message : String(primaryError);
        const rescue = rescueError instanceof Error ? rescueError.message : String(rescueError);
        throw new Error(`${primary} Emergency diagnostic export also failed: ${rescue}`);
      }
    }
  }
  const definition = archiveDefinitions[kind];
  return fetchDirectArchive(definition.endpoint, definition.fallbackName, diagnosticOptions.signal);
}
