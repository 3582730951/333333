import { beforeEach, describe, expect, it, vi } from 'vitest';

const api = vi.hoisted(() => ({
  get: vi.fn(),
  patch: vi.fn(),
  post: vi.fn(),
  statusCode: (error: unknown) => Number((error as { response?: { status?: number } })?.response?.status || 0),
}));

vi.mock('../src/api.js', () => api);

import {
  fetchRegistrationDashboard, fetchRegistrationOptions, saveRegistrationStrategy, startRegistrationJob,
} from '../src/features/automation/api/registration';

function httpError(status: number) {
  return { response: { status } };
}

describe('registration old/new transport compatibility', () => {
  beforeEach(() => {
    api.get.mockReset();
    api.patch.mockReset();
    api.post.mockReset();
  });

  it('falls back from provider options to the legacy provider inventory', async () => {
    api.get.mockImplementation(async (url: string) => {
      if (url === '/admin/groups') return ['cyber'];
      if (url === '/admin/egress-pools') return [{ id: 'registration-pool', purpose: 'registration' }];
      if (url === '/admin/register/providers/options') throw httpError(404);
      if (url === '/admin/register/providers') {
        return { providers: [{ provider_type: 'mail', provider_key: 'email_pool', display_name: '邮箱池', enabled: 1 }] };
      }
      throw new Error(`unexpected URL ${url}`);
    });

    const result = await fetchRegistrationOptions();
    expect(result.error).toBeNull();
    expect(result.groups).toEqual([{ name: 'cyber' }]);
    expect(result.providers.mailbox).toEqual([{ label: '邮箱池', value: 'email_pool' }]);
  });

  it('falls back to legacy jobs while adapting legacy readiness fields', async () => {
    api.get.mockImplementation(async (url: string) => {
      if (url === '/admin/register/batch') throw httpError(410);
      if (url === '/admin/register/email/jobs') return { tasks: [{ jobId: 'old-job', state: 'running' }] };
      if (url === '/admin/register/readiness') return { is_ready: 'true', provider_counts: { mail: 1 } };
      throw new Error(`unexpected URL ${url}`);
    });

    const dashboard = await fetchRegistrationDashboard();
    expect(dashboard.jobs).toEqual([{ id: 'old-job', status: 'running' }]);
    expect(dashboard.readiness).toMatchObject({ ready: true, providers: { mailbox: 1 } });
  });

  it('uses legacy save/start endpoints only when the current endpoint is unsupported', async () => {
    api.post.mockImplementation(async (url: string, body: unknown) => {
      if (url === '/admin/settings-center' || url === '/admin/register/batch') throw httpError(404);
      if (url === '/admin/register/email/start') return { job_id: 'legacy-start', body };
      throw new Error(`unexpected POST ${url}`);
    });
    api.patch.mockResolvedValue({ saved: true });

    await expect(saveRegistrationStrategy({ strategy: 'manual', manualCountry: 'BR', minPrice: 0.1, maxPrice: 0.5 }))
      .resolves.toEqual({ saved: true });
    expect(api.patch).toHaveBeenCalledWith('/admin/config', {
      sms_platform_strategy: 'manual', sms_manual_country: 'BR', sms_min_price: 0.1, sms_max_price: 0.5,
    });

    const started = await startRegistrationJob({
      count: 2, group_name: 'cyber', method: 'protocol_v2', registration_egress_pool_id: 'registration-pool',
      sms_provider: '', mailbox_provider: 'email_pool', identity_mode: 'email', country: '',
    });
    expect(started).toMatchObject({ job_id: 'legacy-start' });
    expect(api.post).toHaveBeenCalledWith('/admin/register/email/start', {
      count: 2, group_name: 'cyber', egress_pool_id: 'registration-pool',
    });
  });
});
