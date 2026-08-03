import { beforeEach, describe, expect, it, vi } from 'vitest';

const api = vi.hoisted(() => ({
  del: vi.fn(),
  get: vi.fn(),
  post: vi.fn(),
}));

vi.mock('../src/api.js', () => api);

import { saveRegistrarSettings } from '../src/features/settings/api/settings';

const input = {
  providers: [{ type: 'sms', key: 'smsactivate', enabled: true, config: { api_key: 'secret' } }],
  values: { phoneCountryCode: 'BR' },
};

describe('registrar settings save transport', () => {
  beforeEach(() => {
    api.post.mockReset();
  });

  it('uses the atomic provider and registrar contract when supported', async () => {
    api.post.mockResolvedValue({
      saved: 1,
      registrar_saved: true,
      settings_saved: [{ section: 'registrar', key: 'phoneCountryCode', old_value: 'US', new_value: 'BR' }],
      reload_ok: false,
      warning: 'saved; runtime reload pending',
    });

    await expect(saveRegistrarSettings(input)).resolves.toEqual({
      saved: [{ section: 'registrar', key: 'phoneCountryCode', old_value: 'US', new_value: 'BR' }],
      reloadOk: false,
      warning: 'saved; runtime reload pending',
    });
    expect(api.post).toHaveBeenCalledTimes(1);
    expect(api.post).toHaveBeenCalledWith('/admin/register/providers', {
      providers: input.providers,
      registrar: input.values,
      registrar_mode: 'replace',
    });
  });

  it('finishes with settings-center when an older backend ignores registrar fields', async () => {
    api.post
      .mockResolvedValueOnce({ saved: 1 })
      .mockResolvedValueOnce({ saved: [{ section: 'registrar', key: 'phoneCountryCode', new_value: 'BR' }] });

    await expect(saveRegistrarSettings(input)).resolves.toEqual({
      saved: [{ section: 'registrar', key: 'phoneCountryCode', new_value: 'BR' }],
    });
    expect(api.post).toHaveBeenCalledTimes(2);
  });
});
