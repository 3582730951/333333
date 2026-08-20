import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { get } from '../src/api.js';
import {
  fetchDashboardCore,
  fetchDashboardSecondary,
  invalidateDashboardUsageSnapshot,
} from '../src/features/observability/api/dashboard';

vi.mock('../src/api.js', () => ({
  get: vi.fn(),
  post: vi.fn(),
}));

const accountSummary = {
  total: '9', active: '6', quarantined: '1', cooling: '1', recheck: '1',
  codex: '5', claude: '3', other: '1',
};

function usagePayload(unavailableSections: string[] = []) {
  return {
    timeseries: [{ bucket: '1999998000', requests: '4', total_tokens: '80' }],
    model_series: [{ bucket: '1999998000', series_key: 'codex::gpt-test', total_tokens: '80' }],
    series: [{ series_key: 'codex::gpt-test', series_label: 'Codex · gpt-test' }],
    models: [{ dimension_key: 'codex::gpt-test', display_label: 'Codex · gpt-test', total_tokens: '560' }],
    cache: {
      summary: { cache_input_tokens: '100', cache_read_tokens: '70' },
      by_account: [{ account_id: 'account-a', prompt_tokens: '40' }],
      by_provider: [{ provider: 'codex', cache_input_tokens: '100', cache_read_tokens: '70' }],
      by_provider_model: [{ provider: 'codex', model: 'gpt-test', cache_read_tokens: '70' }],
    },
    unavailable_sections: unavailableSections,
  };
}

function standardResponse(path: string, aggregate = usagePayload()) {
  switch (path) {
    case '/healthz': return { ok: true };
    case '/admin/accounts/summary': return accountSummary;
    case '/admin/register/stats': return { totals: { success_rate: '0.75' }, by_day: [] };
    case '/admin/system': return { supported: true };
    case '/admin/usage/dashboard': return aggregate;
    default: throw new Error(`unexpected request: ${path}`);
  }
}

describe('Dashboard aggregate refresh', () => {
  beforeEach(() => {
    invalidateDashboardUsageSnapshot();
    vi.mocked(get).mockReset();
    vi.useFakeTimers();
    vi.setSystemTime(new Date(2_000_000_000 * 1000));
  });

  afterEach(() => {
    invalidateDashboardUsageSnapshot();
    vi.useRealTimers();
  });

  it('keeps the core paint small and maps usage into the secondary shape with five total GETs', async () => {
    vi.mocked(get).mockImplementation(async (path) => standardResponse(path));

    const [core, secondary] = await Promise.all([
      fetchDashboardCore(),
      fetchDashboardSecondary(),
    ]);

    expect(get).toHaveBeenCalledTimes(5);
    expect(vi.mocked(get).mock.calls.map(([path]) => path).sort()).toEqual([
      '/admin/accounts/summary',
      '/admin/register/stats',
      '/admin/system',
      '/admin/usage/dashboard',
      '/healthz',
    ]);
    const usageCalls = vi.mocked(get).mock.calls.filter(([path]) => path.startsWith('/admin/usage/'));
    expect(usageCalls).toHaveLength(1);
    expect(usageCalls[0][0]).toBe('/admin/usage/dashboard');
    expect(usageCalls[0][1]).toEqual({
      timeseries_since: 2_000_000_000 - 86400,
      models_since: 2_000_000_000 - 7 * 86400,
      bucket: 3600,
      dimension: 'provider_model',
      series_dimension: 'provider_model',
      series_limit: 8,
      fields: 'summary,by_account,by_provider,by_provider_model',
      allow_partial: true,
    });

    expect(core).toMatchObject({
      accountSummary: { total: 9, active: 6, other: 1 },
      health: { ok: true },
      buckets: [],
      modelSeries: [],
      series: [],
      healthAvailable: true,
      timeseriesAvailable: false,
      error: null,
    });
    expect(secondary).toMatchObject({
      registration: { totals: { success_rate: 0.75 } },
      system: { supported: true },
      buckets: [{ bucket: 1_999_998_000, requests: 4, total_tokens: 80 }],
      modelSeries: [{ series_key: 'codex::gpt-test', total_tokens: 80 }],
      series: [{ series_key: 'codex::gpt-test' }],
      timeseriesAvailable: true,
      byModel: [{ dimension_key: 'codex::gpt-test', total_tokens: 560 }],
      cache: { summary: { cache_input_tokens: 100, cache_read_tokens: 70 } },
      registrationAvailable: true,
      systemAvailable: true,
      modelAvailable: true,
      cacheAvailable: true,
      error: null,
    });
  });

  it('retains a successful model result when the aggregate marks only cache unavailable', async () => {
    vi.mocked(get).mockImplementation(async (path) => standardResponse(path, usagePayload(['cache'])));

    const [core, secondary] = await Promise.all([
      fetchDashboardCore(),
      fetchDashboardSecondary(),
    ]);

    expect(core.timeseriesAvailable).toBe(false);
    expect(secondary).toMatchObject({
      timeseriesAvailable: true,
      modelAvailable: true,
      cacheAvailable: false,
      cache: null,
      error: null,
    });
    expect(secondary.byModel).toHaveLength(1);
    expect(vi.mocked(get).mock.calls.filter(([path]) => path === '/admin/usage/dashboard')).toHaveLength(1);
  });

  it('reuses a settled snapshot across drifted timers and invalidates it for one fresh manual refresh', async () => {
    vi.mocked(get).mockImplementation(async (path) => standardResponse(path));

    await fetchDashboardCore();
    vi.advanceTimersByTime(3_000);
    await fetchDashboardSecondary();

    expect(vi.mocked(get).mock.calls.filter(([path]) => path === '/admin/usage/dashboard')).toHaveLength(1);

    invalidateDashboardUsageSnapshot();
    await Promise.all([fetchDashboardCore(), fetchDashboardSecondary()]);

    expect(vi.mocked(get).mock.calls.filter(([path]) => path === '/admin/usage/dashboard')).toHaveLength(2);
  });

  it('keeps optional secondary results while reporting both unavailable usage diagnostics', async () => {
    vi.mocked(get).mockImplementation(async (path) => {
      if (path === '/admin/system') throw new Error('system offline');
      return standardResponse(path, usagePayload(['models', 'cache']));
    });

    const secondary = await fetchDashboardSecondary();

    expect(secondary.registrationAvailable).toBe(true);
    expect(secondary.systemAvailable).toBe(false);
    expect(secondary.modelAvailable).toBe(false);
    expect(secondary.cacheAvailable).toBe(false);
    expect(secondary.error).toMatchObject({
      code: 'DASHBOARD_SECONDARY_UNAVAILABLE',
      retryable: true,
    });
  });

  it('does not hold the core paint open while the secondary usage request is pending', async () => {
    let resolveUsage!: (value: ReturnType<typeof usagePayload>) => void;
    const pendingUsage = new Promise<ReturnType<typeof usagePayload>>((resolve) => {
      resolveUsage = resolve;
    });
    vi.mocked(get).mockImplementation(async (path) => {
      if (path === '/admin/usage/dashboard') return pendingUsage;
      return standardResponse(path);
    });
    const corePromise = fetchDashboardCore();
    const secondaryPromise = fetchDashboardSecondary();
    const usageCall = vi.mocked(get).mock.calls.find(([path]) => path === '/admin/usage/dashboard');
    expect(usageCall).toBeDefined();
    const sharedSignal = (usageCall![2] as { signal: AbortSignal }).signal;

    const core = await corePromise;
    expect(core.timeseriesAvailable).toBe(false);
    expect(sharedSignal.aborted).toBe(false);

    resolveUsage(usagePayload());
    const secondary = await secondaryPromise;
    expect(secondary.timeseriesAvailable).toBe(true);
    expect(secondary.modelAvailable).toBe(true);
    expect(secondary.cacheAvailable).toBe(true);
    expect(sharedSignal.aborted).toBe(false);
    expect(vi.mocked(get).mock.calls.filter(([path]) => path === '/admin/usage/dashboard')).toHaveLength(1);
  });
});
