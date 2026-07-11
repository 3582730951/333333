import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '@playwright/test';

const cases = [
  { path: '/quota', role: 'admin', viewport: { width: 1440, height: 900 } },
  { path: '/cf-events', role: 'admin', viewport: { width: 390, height: 844 } },
  { path: '/keys', role: 'admin', viewport: { width: 1440, height: 900 } },
  { path: '/users', role: 'admin', viewport: { width: 390, height: 844 } },
  { path: '/portal/keys', role: 'user', viewport: { width: 390, height: 844 } },
  { path: '/system', role: 'admin', viewport: { width: 1440, height: 900 } },
  { path: '/usage', role: 'admin', viewport: { width: 390, height: 844 } },
  { path: '/settings-v2', role: 'admin', viewport: { width: 1440, height: 900 } },
  { path: '/thinking', role: 'admin', viewport: { width: 390, height: 844 } },
  { path: '/', role: 'admin', viewport: { width: 1440, height: 900 } },
  { path: '/portal', role: 'user', viewport: { width: 390, height: 844 } },
] as const;

for (const entry of cases) {
  test(`${entry.path} has no WCAG A/AA violations`, async ({ page }) => {
    await page.setViewportSize(entry.viewport);
    await page.route('**/*', async (route) => {
      const url = new URL(route.request().url());
      const api = url.pathname.startsWith('/admin/') || url.pathname.startsWith('/auth/') || url.pathname.startsWith('/user/') || url.pathname === '/healthz';
      if (!api) return route.continue();
      if (url.pathname === '/healthz') return route.fulfill({ json: { ok: true } });
      if (url.pathname === '/auth/me') {
        return route.fulfill({ json: { authed: true, role: entry.role, email: `${entry.role}@example.test` } });
      }
      if (url.pathname === '/admin/system') {
        return route.fulfill({ json: {
          supported: true,
          uptime_seconds: 120,
          cpu: { usage_pct: 12, cores: 4, load1: 0.2 },
          mem: { used_pct: 40, used_kb: 400, total_kb: 1000 },
          disk: { used_pct: 25, used_bytes: 250, total_bytes: 1000, free_bytes: 750 },
          registration: { total_rss_kb: 0, procs: [] },
          go: { goroutines: 10, sys_bytes: 1000 },
          supervisor_events: [], supervisor_modules: [],
        } });
      }
      if (url.pathname === '/admin/accounts/summary') {
        return route.fulfill({ json: { total: 4, active: 2, quarantined: 1, cooling: 1, recheck: 0, codex: 3, claude: 1, other: 0 } });
      }
      if (url.pathname === '/admin/register/stats') {
        return route.fulfill({ json: { totals: { success_rate: 0.75, succeeded: 3, failed: 1 }, by_day: [{ date: '2026-07-11', succeeded: 3, failed: 1 }] } });
      }
      if (url.pathname === '/admin/usage') {
        return route.fulfill({ json: {
          rows: [{ account_id: 'long-accessible-account-identifier-001', label: 'Accessible usage account', requests: 5, prompt_tokens: 800, completion_tokens: 200, total_tokens: 1000 }],
          window: { timezone: 'Asia/Shanghai', utc_offset_seconds: 28800 },
        } });
      }
      if (url.pathname === '/admin/usage/timeseries') {
        return route.fulfill({ json: {
          buckets: [{ bucket: 1_700_000_000, requests: 5, prompt_tokens: 800, completion_tokens: 200, total_tokens: 1000 }],
          model_series: [{ bucket: 1_700_000_000, series_key: 'gpt-5', total_tokens: 1000 }],
          series: [{ series_dimension: 'model', series_key: 'gpt-5', series_label: 'GPT-5' }],
        } });
      }
      if (url.pathname === '/admin/usage/by-model') {
        return route.fulfill({ json: { models: [{ model: 'gpt-5', cache_input_tokens: 800, cache_read_tokens: 600, total_tokens: 1000 }] } });
      }
      if (url.pathname === '/admin/usage/cache') {
        return route.fulfill({ json: {
          summary: { requests: 5, hit_requests: 3, request_hit_rate: 0.6, cache_input_tokens: 800, cache_read_tokens: 600, cache_creation_tokens: 100 },
          by_model: [{ model: 'gpt-5', cache_input_tokens: 800, cache_read_tokens: 600, total_tokens: 1000 }],
          by_api_key: [{ api_key_hash_prefix: 'key-a11y', requests: 5, request_hit_rate: 0.6 }],
          by_account_model: [], by_route: [], by_route_account_model: [], by_time_bucket: [],
        } });
      }
      if (url.pathname === '/admin/config') {
        return route.fulfill({ json: [
          { key: 'require_downstream_key', label: '要求下游 Key', category: '安全', type: 'bool', value: false, effect: 'hot' },
          { key: 'registration_concurrency', label: '注册并发数', category: '注册', type: 'int', value: 2, effect: 'restart', help: '重启后生效' },
        ] });
      }
      if (url.pathname === '/admin/thinking') {
        return route.fulfill({ json: {
          enabled: true, default_mode: 'level', default_level: 'medium', default_budget: 4096,
          providers: { anthropic: { mode: 'budget', budget: 8192 } }, models: {},
        } });
      }
      if (url.pathname === '/admin/moderation') {
        return route.fulfill({ json: { enabled: false, model: 'gpt-5-mini', auto_translate: true, words: ['secret'] } });
      }
      if (url.pathname === '/user/usage') {
        return route.fulfill({ json: [{ model: 'gpt-5-long-model-name', model_key: 'gpt-5', model_label: 'GPT-5', requests: 4, prompt_tokens: 800, completion_tokens: 200, total_tokens: 1000 }] });
      }
      if (url.pathname === '/user/usage/timeseries') {
        return route.fulfill({ json: { buckets: [{ bucket: 1_700_000_000, requests: 4, prompt_tokens: 800, completion_tokens: 200, total_tokens: 1000 }] } });
      }
      return route.fulfill({ json: [] });
    });
    await page.goto(`/console${entry.path}`);
    await expect(page.locator('[data-page-ready="true"]')).toBeVisible();
    const results = await new AxeBuilder({ page }).withTags(['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa']).analyze();
    expect(results.violations, results.violations.map((violation) => `${violation.id}: ${violation.help}`).join('\n')).toEqual([]);
  });
}
