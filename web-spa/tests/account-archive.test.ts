import { beforeEach, describe, expect, it, vi } from 'vitest';
import api from '../src/api.js';
import {
  fetchAccountArchive,
  importAccountArchive,
} from '../src/features/accounts/api/accounts';

vi.mock('../src/api.js', () => ({
  default: {
    get: vi.fn(),
    post: vi.fn(),
  },
  get: vi.fn(),
}));

describe('account archive API', () => {
  beforeEach(() => {
    vi.mocked(api.get).mockReset();
    vi.mocked(api.post).mockReset();
  });

  it('downloads one selected account as a validated JSON document', async () => {
    const payload = new Blob([
      JSON.stringify({ type: 'codex-account-pool-account', version: 1, account: { id: 'acc-a' } }),
    ], { type: 'application/json' });
    vi.mocked(api.get).mockResolvedValue({
      data: payload,
      headers: {
        'content-type': 'application/json; charset=utf-8',
        'content-disposition': 'attachment; filename="account-acc-a.json"',
      },
    });

    const archive = await fetchAccountArchive(['acc-a', 'acc-a', '']);

    expect(api.get).toHaveBeenCalledWith('/admin/accounts/export', {
      params: { format: 'backup', ids: 'acc-a' },
      responseType: 'blob',
      timeout: 1_800_000,
    });
    expect(archive.filename).toBe('account-acc-a.json');
    expect(archive.blob).toBe(payload);
  });

  it('downloads all or multiple accounts as a ZIP and verifies its signature', async () => {
    const zip = new Blob([
      Uint8Array.from([0x50, 0x4b, 0x03, 0x04, 0x14, 0x00]),
    ], { type: 'application/zip' });
    vi.mocked(api.get).mockResolvedValue({
      data: zip,
      headers: {
        'content-type': 'application/zip',
        'content-disposition': "attachment; filename*=UTF-8''account-pool.zip",
      },
    });

    const archive = await fetchAccountArchive([]);

    expect(api.get).toHaveBeenCalledWith('/admin/accounts/export', {
      params: { format: 'backup' },
      responseType: 'blob',
      timeout: 1_800_000,
    });
    expect(archive.filename).toBe('account-pool.zip');
    expect(archive.blob.size).toBe(6);
  });

  it('uploads a local JSON or ZIP as multipart and normalizes the full-restore summary', async () => {
    const file = new File(['{"version":1}'], 'account.json', { type: 'application/json' });
    vi.mocked(api.post).mockResolvedValue({
      data: {
        recognized: 1,
        imported: 0,
        replaced: 1,
        files: 1,
        zip: false,
        formats: ['pool-account-v1'],
        accounts: [{ id: 'acc-a', label: 'A', provider: 'codex', status: 'imported' }],
      },
    });

    const result = await importAccountArchive(file);

    expect(result).toMatchObject({ recognized: 1, imported: 0, replaced: 1, files: 1, zip: false });
    expect(api.post).toHaveBeenCalledTimes(1);
    const [endpoint, body, config] = vi.mocked(api.post).mock.calls[0];
    expect(endpoint).toBe('/admin/accounts/import-archive');
    expect(body).toBeInstanceOf(FormData);
    const uploaded = (body as FormData).get('file');
    expect(uploaded).toBeInstanceOf(File);
    expect(uploaded).toMatchObject({
      name: file.name,
      type: file.type,
      size: file.size,
    });
    expect(await (uploaded as File).text()).toBe(await file.text());
    expect(config).toEqual({ timeout: 1_800_000 });
  });

  it('forwards abort signals to account archive downloads and uploads', async () => {
    const controller = new AbortController();
    vi.mocked(api.get).mockResolvedValue({
      data: new Blob(['{}'], { type: 'application/json' }),
      headers: { 'content-type': 'application/json' },
    });
    vi.mocked(api.post).mockResolvedValue({
      data: {
        recognized: 1,
        imported: 1,
        replaced: 0,
        files: 1,
        zip: false,
        formats: ['pool-account-v1'],
        accounts: [{ id: 'acc-a', status: 'imported' }],
      },
    });

    await fetchAccountArchive(['acc-a'], controller.signal);
    await importAccountArchive(new File(['{}'], 'account.json'), controller.signal);

    expect(api.get).toHaveBeenCalledWith('/admin/accounts/export', expect.objectContaining({
      signal: controller.signal,
      timeout: 1_800_000,
    }));
    expect(api.post).toHaveBeenCalledWith(
      '/admin/accounts/import-archive',
      expect.any(FormData),
      { signal: controller.signal, timeout: 1_800_000 },
    );
  });

  it('does not save JSON/HTML error bodies as account archives', async () => {
    vi.mocked(api.get).mockResolvedValue({
      data: new Blob(['{"error":"busy"}'], { type: 'application/json' }),
      headers: { 'content-type': 'text/html' },
    });
    await expect(fetchAccountArchive(['acc-a'])).rejects.toThrow('未返回账号 JSON 或 ZIP');

    vi.mocked(api.get).mockResolvedValue({
      data: new Blob(['not-json'], { type: 'application/json' }),
      headers: { 'content-type': 'application/json' },
    });
    await expect(fetchAccountArchive(['acc-a'])).rejects.toThrow('JSON 备份无效');
  });

  it('rejects empty local files before issuing a request', async () => {
    await expect(importAccountArchive(new File([], 'empty.zip'))).rejects.toThrow('非空');
    expect(api.post).not.toHaveBeenCalled();
  });
});
