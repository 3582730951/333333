import { beforeEach, describe, expect, it, vi } from 'vitest';
import api from '../src/api.js';
import { fetchAuditArchive } from '../src/features/observability/api/exports';

vi.mock('../src/api.js', () => ({
  default: {
    get: vi.fn(),
    post: vi.fn(),
    delete: vi.fn(),
  },
}));

const zipPayload = () => Uint8Array.from([
  0x50, 0x4b, 0x03, 0x04, 0x14, 0x00, 0x00, 0x00,
]).buffer;

describe('diagnostic archive export', () => {
  beforeEach(() => {
    vi.mocked(api.get).mockReset();
    vi.mocked(api.post).mockReset();
    vi.mocked(api.delete).mockReset();
    vi.mocked(api.delete).mockResolvedValue({ data: undefined });
  });

  it('creates, polls, and downloads only a ready diagnostic job', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: { job: { id: 'diagjob_test123', status: 'queued' } },
    });
    vi.mocked(api.get)
      .mockResolvedValueOnce({ data: { id: 'diagjob_test123', status: 'validating' } })
      .mockResolvedValueOnce({
        data: {
          id: 'diagjob_test123',
          status: 'ready',
          download_url: '/admin/diagnostics/jobs/diagjob_test123/download',
        },
      })
      .mockResolvedValueOnce({
        data: zipPayload(),
        headers: {
          'content-type': 'application/zip',
          'content-disposition': 'attachment; filename="diagnostics-v3.zip"',
        },
      });

    const archive = await fetchAuditArchive('diagnostics', { pollIntervalMs: 0 });

    expect(api.post).toHaveBeenCalledWith('/admin/diagnostics/jobs', {});
    expect(api.get).toHaveBeenNthCalledWith(1, '/admin/diagnostics/jobs/diagjob_test123');
    expect(api.get).toHaveBeenNthCalledWith(2, '/admin/diagnostics/jobs/diagjob_test123');
    expect(api.get).toHaveBeenNthCalledWith(3, '/admin/diagnostics/jobs/diagjob_test123/download', {
      responseType: 'blob',
      timeout: 5 * 60 * 1_000,
    });
    expect(archive.filename).toBe('diagnostics-v3.zip');
    expect(Array.from(new Uint8Array(await archive.blob.slice(0, 4).arrayBuffer()))).toEqual([
      0x50, 0x4b, 0x03, 0x04,
    ]);
  });

  it('does not turn a JSON response into a ZIP file', async () => {
    vi.mocked(api.get).mockResolvedValue({
      data: new TextEncoder().encode('{"job":{"status":"queued"}}').buffer,
      headers: { 'content-type': 'application/json' },
    });

    await expect(fetchAuditArchive('cache-hits')).rejects.toThrow('did not return');
  });

  it('uses the same-origin rescue stream when generation fails', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: { job: { id: 'diagjob_failed', status: 'queued' } },
    });
    vi.mocked(api.get)
      .mockResolvedValueOnce({
        data: { id: 'diagjob_failed', status: 'failed', error_code: 'dlp_validation_failed' },
      })
      .mockResolvedValueOnce({
        data: zipPayload(),
        headers: { 'content-type': 'application/zip' },
      });

    const archive = await fetchAuditArchive('diagnostics', { pollIntervalMs: 0 });

    expect(archive.blob.size).toBeGreaterThan(0);
    expect(api.get).toHaveBeenNthCalledWith(2, '/admin/export/logs?mode=rescue', {
      responseType: 'blob',
      timeout: 5 * 60 * 1_000,
    });
  });

  it('cancels a job when the client-side deadline expires', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: { job: { id: 'diagjob_slow', status: 'queued' } },
    });
    vi.mocked(api.delete).mockResolvedValue({ data: undefined });
    let clock = 9;

    await expect(fetchAuditArchive('diagnostics', {
      pollIntervalMs: 0,
      timeoutMs: 1,
      queueTimeoutMs: 1,
      now: () => ++clock,
    })).rejects.toThrow('timed out');
    expect(api.get).toHaveBeenCalledWith('/admin/export/logs?mode=rescue', {
      responseType: 'blob',
      timeout: 60_000,
    });
    expect(api.delete).toHaveBeenCalledWith('/admin/diagnostics/jobs/diagjob_slow');
  });

  it('cancels and reports a queued job when no worker claims it', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: { job: { id: 'diagjob_stranded', status: 'queued' } },
    });
    vi.mocked(api.get).mockResolvedValue({
      data: { id: 'diagjob_stranded', status: 'queued' },
    });
    vi.mocked(api.delete).mockResolvedValue({ data: undefined });
    let clock = 0;

    await expect(fetchAuditArchive('diagnostics', {
      pollIntervalMs: 0,
      timeoutMs: 30 * 60 * 1_000,
      queuedStallMs: 60_000,
      now: () => {
        clock += 30_001;
        return clock;
      },
    })).rejects.toThrow('did not claim');

    expect(api.get).toHaveBeenCalledTimes(3);
    expect(api.get).toHaveBeenNthCalledWith(2, '/admin/diagnostics/jobs');
    expect(api.get).toHaveBeenNthCalledWith(3, '/admin/export/logs?mode=rescue', {
      responseType: 'blob',
      timeout: 30 * 60 * 1_000,
    });
    expect(api.delete).toHaveBeenCalledWith('/admin/diagnostics/jobs/diagjob_stranded');
  });

  it('keeps a legitimately queued job while another export is rendering', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: { job: { id: 'diagjob_waiting', status: 'queued' } },
    });
    vi.mocked(api.get)
      .mockResolvedValueOnce({
        data: { id: 'diagjob_waiting', status: 'queued' },
      })
      .mockResolvedValueOnce({
        data: {
          jobs: [
            { id: 'diagjob_running', status: 'rendering' },
            { id: 'diagjob_waiting', status: 'queued' },
          ],
        },
      })
      .mockResolvedValueOnce({
        data: {
          id: 'diagjob_waiting',
          status: 'ready',
          download_url: '/admin/diagnostics/jobs/diagjob_waiting/download',
        },
      })
      .mockResolvedValueOnce({
        data: zipPayload(),
        headers: { 'content-type': 'application/zip' },
      });
    let clock = 0;

    const archive = await fetchAuditArchive('diagnostics', {
      pollIntervalMs: 0,
      timeoutMs: 30 * 60 * 1_000,
      queuedStallMs: 60_000,
      now: () => {
        clock += 30_001;
        return clock;
      },
    });

    expect(api.get).toHaveBeenNthCalledWith(2, '/admin/diagnostics/jobs');
    expect(api.delete).not.toHaveBeenCalled();
    expect(archive.blob.size).toBeGreaterThan(0);
  });

  it('uses a same-origin server download URL including its query', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: {
        job: {
          id: 'diagjob_signed',
          status: 'ready',
          download_url: '/admin/diagnostics/jobs/diagjob_signed/download?lease=opaque',
        },
      },
    });
    vi.mocked(api.get).mockResolvedValue({
      data: zipPayload(),
      headers: { 'content-type': 'application/zip' },
    });

    await fetchAuditArchive('diagnostics', { pollIntervalMs: 0 });

    expect(api.get).toHaveBeenCalledWith(
      '/admin/diagnostics/jobs/diagjob_signed/download?lease=opaque',
      { responseType: 'blob', timeout: 5 * 60 * 1_000 },
    );
  });

  it('accepts an absolute same-origin server download URL', async () => {
    const downloadURL = `${window.location.origin}/admin/diagnostics/jobs/diagjob_absolute/download?lease=signed`;
    vi.mocked(api.post).mockResolvedValue({
      data: {
        job: {
          id: 'diagjob_absolute',
          status: 'ready',
          download_url: downloadURL,
        },
      },
    });
    vi.mocked(api.get).mockResolvedValue({
      data: zipPayload(),
      headers: { 'content-type': 'application/zip' },
    });

    await fetchAuditArchive('diagnostics', { pollIntervalMs: 0 });

    expect(api.get).toHaveBeenCalledWith(
      '/admin/diagnostics/jobs/diagjob_absolute/download?lease=signed',
      { responseType: 'blob', timeout: 5 * 60 * 1_000 },
    );
  });

  it('rejects a cross-origin job URL, cleans it up, and uses rescue mode', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: {
        job: {
          id: 'diagjob_redirect',
          status: 'ready',
          download_url: 'https://attacker.invalid/archive.zip',
        },
      },
    });
    vi.mocked(api.delete).mockResolvedValue({ data: undefined });
    vi.mocked(api.get).mockResolvedValue({
      data: zipPayload(),
      headers: { 'content-type': 'application/zip' },
    });

    const archive = await fetchAuditArchive('diagnostics', { pollIntervalMs: 0 });

    expect(archive.blob.size).toBeGreaterThan(0);
    expect(api.get).toHaveBeenCalledWith('/admin/export/logs?mode=rescue', {
      responseType: 'blob',
      timeout: 5 * 60 * 1_000,
    });
    expect(api.delete).toHaveBeenCalledWith('/admin/diagnostics/jobs/diagjob_redirect');
  });

  it('aborts polling on unmount and sends a detached job cancellation', async () => {
    const controller = new AbortController();
    vi.mocked(api.post).mockResolvedValue({
      data: { job: { id: 'diagjob_unmounted', status: 'queued' } },
    });
    vi.mocked(api.delete).mockResolvedValue({ data: undefined });

    await expect(fetchAuditArchive('diagnostics', {
      pollIntervalMs: 1,
      signal: controller.signal,
      wait: async () => controller.abort(),
    })).rejects.toMatchObject({ name: 'AbortError' });

    expect(api.get).not.toHaveBeenCalled();
    expect(api.delete).toHaveBeenCalledWith('/admin/diagnostics/jobs/diagjob_unmounted');
  });

  it('waits for an in-flight create id after unmount, then cancels that job', async () => {
    const controller = new AbortController();
    let finishCreate!: (value: { data: { job: { id: string; status: string } } }) => void;
    vi.mocked(api.post).mockReturnValue(new Promise((resolve) => {
      finishCreate = resolve;
    }));

    const pending = fetchAuditArchive('diagnostics', {
      pollIntervalMs: 1,
      signal: controller.signal,
    });
    controller.abort();
    finishCreate({
      data: { job: { id: 'diagjob_created_during_unmount', status: 'queued' } },
    });

    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
    expect(api.get).not.toHaveBeenCalled();
    expect(api.delete).toHaveBeenCalledWith('/admin/diagnostics/jobs/diagjob_created_during_unmount');
  });

  it('allows a long legitimate queue wait then starts a fresh running deadline', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: { job: { id: 'diagjob_long_queue', status: 'queued' } },
    });
    vi.mocked(api.get)
      .mockResolvedValueOnce({ data: { id: 'diagjob_long_queue', status: 'rendering' } })
      .mockResolvedValueOnce({
        data: {
          id: 'diagjob_long_queue',
          status: 'ready',
          download_url: '/admin/diagnostics/jobs/diagjob_long_queue/download',
        },
      })
      .mockResolvedValueOnce({
        data: zipPayload(),
        headers: { 'content-type': 'application/zip' },
      });
    const times = [0, 6 * 60 * 1_000, 6 * 60 * 1_000 + 1];

    const archive = await fetchAuditArchive('diagnostics', {
      pollIntervalMs: 0,
      queuedStallMs: 10 * 60 * 1_000,
      now: () => times.shift() ?? 6 * 60 * 1_000 + 2,
    });

    expect(archive.blob.size).toBeGreaterThan(0);
    expect(api.delete).not.toHaveBeenCalled();
  });

  it('cancels when the running phase exceeds its own deadline', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: { job: { id: 'diagjob_running_timeout', status: 'rendering' } },
    });
    const times = [0, 5 * 60 * 1_000 + 1];

    await expect(fetchAuditArchive('diagnostics', {
      pollIntervalMs: 0,
      now: () => times.shift() ?? 5 * 60 * 1_000 + 2,
    })).rejects.toThrow('timed out');

    expect(api.get).toHaveBeenCalledWith('/admin/export/logs?mode=rescue', {
      responseType: 'blob',
      timeout: 5 * 60 * 1_000,
    });
    expect(api.delete).toHaveBeenCalledWith('/admin/diagnostics/jobs/diagjob_running_timeout');
  });
});
