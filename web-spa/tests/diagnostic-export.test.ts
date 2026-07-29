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
      responseType: 'arraybuffer',
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

  it('stops without downloading when generation fails', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: { job: { id: 'diagjob_failed', status: 'queued' } },
    });
    vi.mocked(api.get).mockResolvedValue({
      data: { id: 'diagjob_failed', status: 'failed', error_code: 'dlp_validation_failed' },
    });

    await expect(fetchAuditArchive('diagnostics', { pollIntervalMs: 0 }))
      .rejects.toThrow('dlp_validation_failed');
    expect(api.get).toHaveBeenCalledTimes(1);
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
      now: () => ++clock,
    })).rejects.toThrow('timed out');
    expect(api.get).not.toHaveBeenCalled();
    expect(api.delete).toHaveBeenCalledWith('/admin/diagnostics/jobs/diagjob_slow');
  });
});
