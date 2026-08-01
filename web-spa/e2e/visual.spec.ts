import { expect, test, type Page, type Route } from '@playwright/test';
import { adminVisualRoutes, portalVisualRoutes } from '../src/app/routeDefinitions';

type Role = 'admin' | 'user';
type FixtureState = 'ready' | 'loading' | 'failure' | 'partial' | 'permission' | 'interactive' | 'unsupported';

const baseViewports = [
  { name: '1440x900', width: 1440, height: 900 },
  { name: '390x844', width: 390, height: 844 },
];
const extraViewports = [
  { name: '1280x720', width: 1280, height: 720 },
  { name: '360x800', width: 360, height: 800 },
];
const themes = ['light', 'dark'] as const;
const extendedPaths = new Set(['/accounts', '/egress', '/usage', '/system', '/settings-v2']);

function apiFixture(pathname: string, search: URLSearchParams): unknown {
  if (pathname === '/healthz') return { ok: true };
  if (pathname === '/admin/accounts/summary') return { total: 3, active: 2, quarantined: 1, cooling: 0, recheck: 0, codex: 2, claude: 1 };
  if (pathname === '/admin/accounts') return { accounts: [], total: 0 };
  if (pathname === '/admin/email-pool') return { accounts: [], total: 0, page: 1, pageSize: 50, counts: {} };
  if (pathname === '/admin/usage/timeseries' || pathname === '/user/usage/timeseries') return { buckets: [] };
  if (pathname === '/admin/usage/by-model') return { models: [] };
  if (pathname === '/admin/usage/cache') return { summary: {}, by_model: [], by_api_key: [], by_account_model: [], by_route: [], by_route_account_model: [] };
  if (pathname === '/admin/usage/dashboard') return { accounts: [], timeseries: [], models: [], model_series: [], series: [], cache: { summary: {}, by_model: [], by_provider: [], by_provider_model: [] }, window: { timezone: 'Asia/Shanghai', utc_offset_seconds: 28800 } };
  if (pathname === '/admin/usage') return { rows: [], window: { timezone: 'Asia/Shanghai', utc_offset_seconds: 28800 } };
  if (pathname === '/admin/thinking') return { enabled: true, default_mode: 'level', default_level: 'medium', default_budget: 4096, providers: {}, models: {} };
  if (pathname === '/admin/moderation') return { enabled: false, model: 'gpt-5-mini', auto_translate: true, words: [] };
  if (pathname === '/user/usage') return { usage: [] };
  if (pathname === '/admin/register/stats') return { totals: { success_rate: 0, succeeded: 0, failed: 0 }, by_day: [] };
  if (pathname === '/admin/register/readiness') return { ready: false, blockers: ['请先配置注册 Provider'], providers: {}, pool: {} };
  if (pathname === '/admin/register/providers/options') return { sms: [], mailbox: [], captcha: [] };
  if (pathname === '/admin/register/providers') return { providers: [] };
  if (pathname === '/admin/register/countries') return [];
  if (pathname === '/admin/register/batch') return { jobs: [] };
  if (pathname === '/admin/system') return {
    supported: true,
    uptime_seconds: 3600,
    cpu: { usage_pct: 12, cores: 4, load1: 0.2 },
    mem: { used_pct: 42, used_kb: 420000, total_kb: 1000000 },
    disk: { used_pct: 30, used_bytes: 3000000, total_bytes: 10000000 },
    registration: { total_rss_kb: 0, procs: [] },
    go: { goroutines: 18, sys_bytes: 12000000 },
    supervisor_events: [], supervisor_modules: [],
  };
  if (pathname === '/admin/settings-center') {
    const sections = (search.get('sections') || '').split(',');
    const value: Record<string, unknown> = {};
    if (sections.includes('automation')) value.automation = { policies: [], stats: null, readiness: { ready: false, blockers: ['尚未配置'] } };
    if (sections.includes('registrar')) value.registrar = {};
    if (sections.includes('logging')) value.logging = {};
    if (sections.includes('memory')) value.memory = {};
    return value;
  }
  if (pathname === '/admin/config') return [];
  if (pathname === '/admin/upstream-error-rules') return { rules: [] };
  if (pathname === '/admin/upstream-error-rules/model-options') return { families: [] };
  if (pathname === '/admin/model-quality') return { rows: [], summary: {} };
  if (pathname === '/auth/me') return null;
  if (pathname === '/user/profile') return { email: 'user@example.test', name: 'Portal User', role: 'user' };
  return [];
}

async function mockBackend(page: Page, role: Role, state: FixtureState = 'ready') {
  await page.route('**/*', async (route: Route) => {
    const request = route.request();
    const url = new URL(request.url());
    const isApi = url.pathname.startsWith('/admin/') || url.pathname.startsWith('/auth/') || url.pathname.startsWith('/user/') || url.pathname === '/healthz';
    if (!isApi) return route.continue();
    if (url.pathname === '/auth/me') {
      return route.fulfill({ json: { authed: true, role, email: role === 'admin' ? 'admin@example.test' : 'user@example.test' } });
    }
    if (state === 'loading') await new Promise((resolve) => setTimeout(resolve, 650));
    if (state === 'failure' && (url.pathname === '/admin/accounts' || url.pathname === '/admin/accounts/summary')) {
      return route.fulfill({ status: 503, headers: { 'x-request-id': 'e2e-failure-1' }, json: { error: { message: 'fixture network failure', request_id: 'e2e-failure-1' } } });
    }
    if (state === 'failure' && (url.pathname === '/user/usage' || url.pathname === '/user/usage/timeseries')) {
      return route.fulfill({ status: 503, headers: { 'x-request-id': 'e2e-portal-usage-failure-1' }, json: { error: { message: 'portal usage fixture failure', request_id: 'e2e-portal-usage-failure-1' } } });
    }
    if (state === 'partial' && url.pathname === '/admin/system') {
      return route.fulfill({ status: 503, headers: { 'x-request-id': 'e2e-dashboard-partial-1' }, json: { error: { message: 'dashboard secondary fixture failure', request_id: 'e2e-dashboard-partial-1' } } });
    }
    if (state === 'partial' && url.pathname === '/user/usage/timeseries') {
      return route.fulfill({ status: 503, headers: { 'x-request-id': 'e2e-portal-partial-1' }, json: { error: { message: 'portal trend fixture failure', request_id: 'e2e-portal-partial-1' } } });
    }
    if ((state === 'interactive' || state === 'partial') && request.method() === 'GET' && url.pathname === '/user/usage') {
      return route.fulfill({ json: [{ model: 'gpt-5-with-a-very-long-model-identifier', model_key: 'gpt-5', model_label: 'GPT-5 Production', requests: 7, prompt_tokens: 1200, completion_tokens: 300, total_tokens: 1500 }] });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/user/usage/timeseries') {
      return route.fulfill({ json: { buckets: [{ bucket: 1_700_000_000, requests: 7, prompt_tokens: 1200, completion_tokens: 300, total_tokens: 1500 }] } });
    }
    if (state === 'failure' && url.pathname === '/admin/system') {
      return route.fulfill({ status: 503, headers: { 'x-request-id': 'e2e-system-failure-1' }, json: { error: { message: 'system metrics fixture failure', request_id: 'e2e-system-failure-1' } } });
    }
    if (state === 'failure' && url.pathname.startsWith('/admin/usage')) {
      return route.fulfill({ status: 503, headers: { 'x-request-id': 'e2e-usage-failure-1' }, json: { error: { message: 'usage fixture failure', request_id: 'e2e-usage-failure-1' } } });
    }
    if (state === 'failure' && url.pathname === '/admin/config') {
      return route.fulfill({ status: 503, headers: { 'x-request-id': 'e2e-settings-failure-1' }, json: { error: { message: 'settings fixture failure', request_id: 'e2e-settings-failure-1' } } });
    }
    if (state === 'failure' && (url.pathname === '/admin/thinking' || url.pathname === '/admin/moderation')) {
      return route.fulfill({ status: 503, headers: { 'x-request-id': 'e2e-advanced-settings-failure-1' }, json: { error: { message: 'advanced settings fixture failure', request_id: 'e2e-advanced-settings-failure-1' } } });
    }
    if (state === 'unsupported' && url.pathname === '/admin/system') {
      return route.fulfill({ json: { supported: false } });
    }
    if (state === 'permission' && url.pathname.startsWith('/admin/users')) {
      return route.fulfill({ status: 403, json: { error: { message: '权限不足', request_id: 'e2e-permission-1' } } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/accounts') {
      return route.fulfill({ json: { accounts: [{ id: 'account-with-a-very-long-identifier-001', label: 'Primary production account', email: 'operator.with.long.alias@example.test', provider: 'codex', status: 'active', group_name: 'default', usage: { requests: 12, total_tokens: 3456 } }], total: 1 } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/usage/by-model') {
      return route.fulfill({ json: { models: [
        { model: 'gpt-5.5', total_tokens: 3_000_000_000 },
        { model: 'claude-opus-4-8', total_tokens: 220_000_000 },
      ] } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/api-keys') {
      return route.fulfill({ json: [] });
    }
    if (state === 'interactive' && request.method() === 'POST' && url.pathname === '/admin/api-keys') {
      return route.fulfill({ json: { key: 'cap_e2e_new_secret' } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/register/readiness') {
      return route.fulfill({ json: { ready: false, blockers: ['auto-refill 策略未启用'], providers: { sms: 1, mailbox: 1, email_otp: 1, captcha: 0 }, pool: { active: 3, target: 5, deficit: 2 } } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/register/countries') {
      return route.fulfill({ json: [{ isoCode: 'US', nameZh: '美国', name: 'United States' }] });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/config') {
      return route.fulfill({ json: [
        { key: 'sms_platform_strategy', label: '国家策略', category: '注册', type: 'select', options: ['auto', 'manual'], value: 'auto', effect: 'hot' },
        { key: 'default_register_method', label: '默认注册引擎', category: '注册', type: 'select', options: ['node', 'protocol_v2'], value: 'node', effect: 'hot' },
        { key: 'require_downstream_key', label: '要求下游 Key', category: '安全', type: 'bool', value: false, effect: 'hot', future_field: true },
      ] });
    }
    if (state === 'interactive' && request.method() === 'POST' && url.pathname === '/admin/settings-center/apply-template') {
      return route.fulfill({ json: { id: 'optimal-stable-models-v1', name: '全模型稳定推荐配置', description: '测试模板', saved: [{ section: 'config', key: 'require_downstream_key', old_value: false, new_value: true }] } });
    }
    if (state === 'interactive' && request.method() === 'POST' && url.pathname === '/admin/settings-center') {
      const body = request.postDataJSON();
      const patches = Array.isArray(body) ? body : [body];
      const saved = patches.flatMap((patch: { section?: string; values?: Record<string, unknown> }) => Object.entries(patch.values || {}).map(([key, value]) => ({
        section: patch.section || 'config', key, old_value: null, new_value: value,
      })));
      return route.fulfill({ json: { saved } });
    }
    if (state === 'interactive' && request.method() === 'POST' && url.pathname === '/admin/thinking') {
      return route.fulfill({ json: { status: 'ok' } });
    }
    if (state === 'interactive' && request.method() === 'POST' && url.pathname === '/admin/moderation') {
      return route.fulfill({ json: request.postDataJSON() });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/register/batch') {
      return route.fulfill({ json: { jobs: [{ id: 'register-e2e-1', method: 'node', identity_mode: 'phone', status: 'running', total: 2, succeeded: 1, failed: 0 }] } });
    }
    if (state === 'interactive' && request.method() === 'POST' && url.pathname === '/admin/register/batch') {
      return route.fulfill({ json: { id: 'register-e2e-created' } });
    }
    if (state === 'interactive' && request.method() === 'POST' && url.pathname === '/admin/usage/cache/reset') {
      return route.fulfill({ json: { ok: true } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/usage/dashboard') {
      const total = url.searchParams.has('since') ? 7654 : 1234;
      return route.fulfill({ json: {
        accounts: [{
          account_id: 'account-with-a-very-long-usage-identifier-001', label: 'Primary usage account', requests: 12,
          prompt_tokens: total - 234, completion_tokens: 234, total_tokens: total,
          actual_requests: 12, actual_prompt_tokens: total - 234, actual_completion_tokens: 234,
          actual_total_tokens: total, combined_total_tokens: total,
        }],
        timeseries: [{ bucket: 1_700_000_000, requests: 12, prompt_tokens: total - 234, completion_tokens: 234, total_tokens: total, cache_read_tokens: 400, cache_creation_tokens: 100 }],
        models: [{ model: 'gpt-5', model_key: 'gpt-5', model_label: 'GPT-5', requests: 12, prompt_tokens: total - 234, total_tokens: total, cache_input_tokens: 500, cache_read_tokens: 400 }],
        model_series: [{ bucket: 1_700_000_000, series_key: 'gpt-5', series_label: 'GPT-5', requests: 12, prompt_tokens: total - 234, completion_tokens: 234, total_tokens: total, cache_read_tokens: 400 }],
        series: [{ series_dimension: 'model', series_key: 'gpt-5', series_label: 'GPT-5' }],
        cache: {
          summary: { requests: 12, hit_requests: 8, request_hit_rate: 2 / 3, cache_input_tokens: 500, cache_read_tokens: 400, cache_creation_tokens: 100 },
          by_model: [{ model: 'gpt-5', cache_input_tokens: 500, cache_read_tokens: 400, total_tokens: total }],
          by_provider: [], by_provider_model: [],
        },
        window: { timezone: 'Asia/Shanghai', utc_offset_seconds: 28800, next_day_start_at: 1_700_086_400 },
        effective_start_at: 1_700_000_000, effective_until_at: 1_700_086_400,
      } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/usage') {
      const total = url.searchParams.has('since') ? 7654 : 1234;
      return route.fulfill({ json: {
        rows: [{ account_id: 'account-with-a-very-long-usage-identifier-001', label: 'Primary usage account', requests: 12, prompt_tokens: total - 234, completion_tokens: 234, total_tokens: total }],
        window: { timezone: 'Asia/Shanghai', utc_offset_seconds: 28800, next_day_start_at: 1_700_086_400 },
        effective_start_at: 1_700_000_000, effective_until_at: 1_700_086_400,
      } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/usage/timeseries') {
      const total = url.searchParams.has('since') ? 7654 : 1234;
      return route.fulfill({ json: {
        buckets: [{ bucket: 1_700_000_000, requests: 12, prompt_tokens: total - 234, completion_tokens: 234, total_tokens: total, cache_read_tokens: 400, cache_creation_tokens: 100 }],
        model_series: [{ bucket: 1_700_000_000, series_key: 'gpt-5', series_label: 'GPT-5', requests: 12, prompt_tokens: total - 234, completion_tokens: 234, total_tokens: total, cache_read_tokens: 400 }],
        series: [{ series_dimension: 'model', series_key: 'gpt-5', series_label: 'GPT-5' }],
      } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/usage/by-model') {
      return route.fulfill({ json: { models: [{ model: 'gpt-5', model_key: 'gpt-5', model_label: 'GPT-5', requests: 12, prompt_tokens: 1000, total_tokens: 1234, cache_input_tokens: 500, cache_read_tokens: 400 }] } });
    }
    if (state === 'interactive' && request.method() === 'GET' && url.pathname === '/admin/usage/cache') {
      return route.fulfill({ json: {
        summary: { requests: 12, hit_requests: 8, request_hit_rate: 0.6667, prompt_tokens: 1000, cache_input_tokens: 500, cache_read_tokens: 400, cache_creation_tokens: 100, cache_miss_tokens: 0, real_token_hit_rate: 0.4, eligible_cache_hit_rate: 0.8, cache_write_share: 0.1 },
        by_model: [{ model: 'gpt-5', model_key: 'gpt-5', model_label: 'GPT-5', requests: 12, cache_input_tokens: 500, cache_read_tokens: 400, total_tokens: 1234, latest_user_cache_control: 2 }],
        by_api_key: [{ api_key_hash_prefix: 'key-e2e', requests: 12, hit_requests: 8, request_hit_rate: 0.6667, cache_read_tokens: 400 }],
        by_account_model: [], by_route: [], by_route_account_model: [], by_time_bucket: [],
        effective_start_at: 1_700_000_000,
      } });
    }
    return route.fulfill({ status: 200, json: apiFixture(url.pathname, url.searchParams) });
  });
}

function slug(value: string) {
  return value.replace(/^\//, '').replace(/[^a-z0-9]+/gi, '-') || 'dashboard';
}

function watchRuntimeIssues(page: Page) {
  const issues: string[] = [];
  page.on('pageerror', (error) => issues.push(`pageerror: ${error.message}`));
  page.on('console', (message) => {
    if (message.type() === 'error') issues.push(`console.error: ${message.text()}`);
  });
  return issues;
}

async function expectHealthyRender(page: Page, issues: string[], context: string) {
  await expect(page.locator('.pool-error-boundary'), `${context} rendered the application error boundary`).toHaveCount(0);
  expect(issues, `${context} emitted browser runtime errors:\n${issues.join('\n')}`).toEqual([]);
}

async function capture(page: Page, routePath: string, viewport: { name: string; width: number; height: number }, theme: 'light' | 'dark', outputPath: (name: string) => string) {
  await page.setViewportSize(viewport);
  await page.addInitScript((selectedTheme) => localStorage.setItem('pool_theme', selectedTheme), theme);
  await page.goto(`/console${routePath}`, { waitUntil: 'domcontentloaded' });
  await expect(page.locator('[data-page-ready="true"]')).toBeVisible();
  await expect.poll(() => page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: outputPath(`${slug(routePath)}-${viewport.name}-${theme}.png`), fullPage: true, animations: 'disabled' });
}

for (const entry of [...adminVisualRoutes, ...portalVisualRoutes]) {
  const role: Role = entry.path.startsWith('/portal') ? 'user' : 'admin';
  test(`${entry.name} ${entry.path} visual matrix`, async ({ page }, testInfo) => {
    const runtimeIssues = watchRuntimeIssues(page);
    await mockBackend(page, role);
    const viewports = extendedPaths.has(entry.path) ? [...baseViewports, ...extraViewports] : baseViewports;
    for (const viewport of viewports) {
      for (const theme of themes) {
        await capture(page, entry.path, viewport, theme, testInfo.outputPath.bind(testInfo));
        await expectHealthyRender(page, runtimeIssues, `${entry.path} at ${viewport.name} in ${theme} mode`);
      }
    }
  });
}

test('loading, failure, permission, and settings-query states are explicit', async ({ browser }, testInfo) => {
  const loadingPage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(loadingPage, 'admin', 'loading');
  await loadingPage.goto('/console/accounts');
  await expect(loadingPage.locator('.pool-app-layout')).toBeVisible();
  await expect(loadingPage.locator('[data-page-ready="false"]')).toBeVisible();
  await loadingPage.screenshot({ path: testInfo.outputPath('state-loading.png'), animations: 'disabled' });
  await loadingPage.close();

  const failurePage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(failurePage, 'admin', 'failure');
  await failurePage.goto('/console/accounts');
  await expect(failurePage.getByRole('alert')).toContainText('数据读取失败');
  await expect(failurePage.getByRole('alert')).toContainText('e2e-failure-1');
  await expect(failurePage.getByText('共 0 个账号')).toHaveCount(0);
  await failurePage.screenshot({ path: testInfo.outputPath('state-failure.png'), animations: 'disabled' });
  await failurePage.close();

  const permissionPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(permissionPage, 'admin', 'permission');
  await permissionPage.goto('/console/users');
  await expect(permissionPage.getByText('权限不足')).toBeVisible();
  await permissionPage.screenshot({ path: testInfo.outputPath('state-permission.png'), animations: 'disabled' });
  await permissionPage.close();

  const settingsPage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(settingsPage, 'admin');
  await settingsPage.goto('/console/thinking');
  await expect(settingsPage).toHaveURL(/settings-v2\?tab=thinking/);
  await expect(settingsPage.getByRole('tab', { name: '思考配置' })).toHaveAttribute('data-state', 'active');
  await settingsPage.close();
});

test('submit, success, download, batch selection, and confirmation remain usable', async ({ browser }) => {
  const keyPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(keyPage, 'admin', 'interactive');
  await keyPage.goto('/console/keys');
  await keyPage.getByRole('button', { name: '创建 Key' }).click();
  const dialog = keyPage.getByRole('dialog', { name: '创建 API Key' });
  await dialog.locator('input').first().fill('e2e key');
  await dialog.getByRole('button', { name: '创建', exact: true }).click();
  await expect(keyPage.getByText('cap_e2e_new_secret', { exact: true })).toBeVisible();
  await expect(keyPage.getByText('Key 已创建', { exact: true })).toBeVisible();
  await keyPage.close();

  const downloadPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(downloadPage, 'admin');
  await downloadPage.goto('/console/quota');
  const downloadPromise = downloadPage.waitForEvent('download');
  await downloadPage.getByRole('button', { name: '导出 CSV' }).click();
  expect((await downloadPromise).suggestedFilename()).toBe('quota.csv');
  await downloadPage.close();

  const batchPage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(batchPage, 'admin', 'interactive');
  await batchPage.goto('/console/accounts');
  await expect(batchPage.locator('[data-page-ready="true"]')).toBeVisible();
  await batchPage.getByRole('button', { name: '选择', exact: true }).click();
  await batchPage.getByRole('checkbox', { name: '选择 Primary production account' }).check();
  await expect(batchPage.getByText('已选 1 项')).toBeVisible();
  await batchPage.getByRole('button', { name: '批量删除' }).click();
  await expect(batchPage.getByText('删除选中的 1 个账号？')).toBeVisible();
  await batchPage.getByRole('button', { name: '取消' }).click();
  await batchPage.close();
});

test('account import provider marks stay bounded and model donut has one readable center label', async ({ browser }) => {
  const accountPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(accountPage, 'admin', 'interactive');
  await accountPage.goto('/console/accounts');
  await accountPage.getByRole('button', { name: '添加账号', exact: true }).click();
  const dialog = accountPage.getByRole('dialog', { name: '添加账号' });
  await expect(dialog).toBeVisible();
  const boundedMarks = await dialog.locator('.pool-vendor-logo__mark').evaluateAll((marks) => marks.map((mark) => {
    const box = mark.getBoundingClientRect();
    return { width: box.width, height: box.height };
  }));
  expect(boundedMarks.length).toBeGreaterThanOrEqual(4);
  expect(boundedMarks.every((mark) => mark.width <= 40 && mark.height <= 40)).toBe(true);
  await dialog.getByRole('tab').filter({ hasText: 'Kiro' }).click();
  await expect(dialog.getByText('Kiro API Key', { exact: true })).toBeVisible();
  await dialog.getByRole('button', { name: '凭证 JSON', exact: true }).click();
  await expect(dialog.getByText('Token / 账号 JSON', { exact: true })).toBeVisible();
  await expect(dialog.getByText('客户端注册 JSON', { exact: true })).toBeVisible();
  await expect(dialog.locator('textarea.pool-textarea')).toHaveCount(2);
  await expect.poll(() => dialog.locator('.pool-modal-body').evaluate((body) => body.scrollWidth <= body.clientWidth)).toBe(true);
  await accountPage.close();

  const dashboardPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(dashboardPage, 'admin', 'interactive');
  await dashboardPage.goto('/console/');
  await expect(dashboardPage.getByText('3.22B', { exact: true })).toHaveCount(1);
  await dashboardPage.close();
});

test('access and audit pages switch locale without remounting', async ({ browser }) => {
  const adminPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(adminPage, 'admin', 'interactive');
  await adminPage.goto('/console/users');
  await expect(adminPage.getByRole('heading', { name: '用户管理' })).toBeVisible();
  await adminPage.getByRole('button', { name: '语言' }).click();
  await expect(adminPage.getByRole('heading', { name: 'User Management' })).toBeVisible();
  await adminPage.goto('/console/keys');
  await expect(adminPage.getByRole('button', { name: 'Create key' })).toBeVisible();
  await adminPage.goto('/console/audit');
  await expect(adminPage.getByRole('heading', { name: 'Audit Log' })).toBeVisible();
  await adminPage.goto('/console/registration');
  await expect(adminPage.getByRole('heading', { name: 'Auto-Registration' })).toBeVisible();
  await expect(adminPage.getByRole('button', { name: 'Start' })).toBeVisible();
  await adminPage.goto('/console/system');
  await expect(adminPage.getByRole('heading', { name: 'System Monitor' })).toBeVisible();
  await expect(adminPage.getByText('CPU usage', { exact: true })).toBeVisible();
  await adminPage.goto('/console/settings-v2');
  await expect(adminPage.getByRole('heading', { name: 'Settings', exact: true })).toBeVisible();
  await expect(adminPage.getByRole('tab', { name: 'General' })).toBeVisible();
  await expect(adminPage.getByRole('button', { name: 'Apply recommended model settings' })).toBeVisible();
  await adminPage.getByRole('tab', { name: 'Reasoning' }).click();
  await expect(adminPage.getByRole('heading', { name: 'Reasoning', exact: true })).toBeVisible();
  await expect(adminPage.getByRole('button', { name: 'Save', exact: true })).toBeVisible();
  await adminPage.goto('/console/');
  await expect(adminPage.getByRole('heading', { name: 'Overview', exact: true })).toBeVisible();
  await expect(adminPage.getByRole('button', { name: 'Import account' })).toBeVisible();
  await adminPage.close();

  const portalPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(portalPage, 'user');
  await portalPage.goto('/console/portal/keys');
  await portalPage.getByRole('button', { name: '语言' }).click();
  await expect(portalPage.getByRole('heading', { name: 'My API Keys' })).toBeVisible();
  await expect(portalPage.getByRole('button', { name: 'New key' })).toBeVisible();
  await portalPage.goto('/console/portal');
  await expect(portalPage.getByRole('heading', { name: 'My Usage', exact: true })).toBeVisible();
  await expect(portalPage.getByText('Last 7 days', { exact: true })).toBeVisible();
  await portalPage.close();
});

test('registration page renders without React console errors', async ({ page }) => {
  const consoleErrors: string[] = [];
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text());
  });
  await mockBackend(page, 'admin', 'interactive');
  await page.goto('/console/registration');
  await expect(page.locator('[data-page-ready="true"]')).toBeVisible();
  await page.getByLabel('国家策略').selectOption('manual');
  await expect(page.getByLabel('指定国家')).toBeVisible();
  expect(consoleErrors).toEqual([]);
});

test('registration saves manual country strategy before starting a job', async ({ page }) => {
  await mockBackend(page, 'admin', 'interactive');
  await page.goto('/console/registration');
  await expect(page.locator('[data-page-ready="true"]')).toBeVisible();
  const startButton = page.getByRole('button', { name: '启动', exact: true });
  await expect(startButton).toBeEnabled();

  await page.getByLabel('国家策略').selectOption('manual');
  await page.getByLabel('指定国家').selectOption('US');
  const strategyRequest = page.waitForRequest((request) => request.method() === 'POST' && new URL(request.url()).pathname === '/admin/settings-center');
  const startRequest = page.waitForRequest((request) => request.method() === 'POST' && new URL(request.url()).pathname === '/admin/register/batch');
  await startButton.click();

  expect((await strategyRequest).postDataJSON()).toEqual([{
    section: 'config', values: { sms_platform_strategy: 'manual', sms_manual_country: 'US', sms_min_price: 0, sms_max_price: 0 },
  }]);
  expect((await startRequest).postDataJSON()).toMatchObject({
    count: 1, method: '', identity_mode: 'phone', country: 'US',
  });
  await expect(page.getByText('国家策略已保存', { exact: true })).toBeVisible();
  await expect(page.getByText('注册任务已启动', { exact: true })).toBeVisible();
});

test('system unsupported and failure states are explicit', async ({ browser }) => {
  const unsupportedPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(unsupportedPage, 'admin', 'unsupported');
  await unsupportedPage.goto('/console/system');
  await expect(unsupportedPage.getByText(/系统指标不可用/)).toBeVisible();
  await expect(unsupportedPage.getByText('CPU 使用率', { exact: true })).toHaveCount(0);
  await unsupportedPage.close();

  const failurePage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(failurePage, 'admin', 'failure');
  await failurePage.goto('/console/system');
  await expect(failurePage.getByRole('alert')).toContainText('系统指标读取失败');
  await expect(failurePage.getByRole('alert')).toContainText('e2e-system-failure-1');
  await expect(failurePage.getByText('CPU 使用率', { exact: true })).toHaveCount(0);
  await failurePage.close();
});

test('usage range, reset, locale, and first-load failure states are explicit', async ({ browser }) => {
  const usagePage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(usagePage, 'admin', 'interactive');
  let dashboardRequests = 0;
  usagePage.on('request', (request) => {
    const url = new URL(request.url());
    if (request.method() === 'GET' && url.pathname === '/admin/usage/dashboard') dashboardRequests += 1;
  });
  await usagePage.goto('/console/usage');
  await expect(usagePage.locator('[data-page-ready="true"]')).toBeVisible();
  await expect(usagePage.locator('.pool-stat').filter({ hasText: '实际 Token' }).locator('.value')).toHaveText('1234');

  const rangeRequest = usagePage.waitForRequest((request) => {
    const url = new URL(request.url());
    return request.method() === 'GET' && url.pathname === '/admin/usage/dashboard' && url.searchParams.has('since');
  });
  await usagePage.getByLabel('窗口').selectOption('604800');
  await rangeRequest;
  await expect(usagePage.locator('.pool-stat').filter({ hasText: '实际 Token' }).locator('.value')).toHaveText('7654');

  const beforeReset = dashboardRequests;
  await usagePage.getByRole('button', { name: '重置用量统计视图' }).click();
  const resetRequest = usagePage.waitForRequest((request) => request.method() === 'POST' && new URL(request.url()).pathname === '/admin/usage/cache/reset');
  await usagePage.getByRole('button', { name: '重置视图', exact: true }).click();
  await resetRequest;
  await expect(usagePage.getByText('缓存统计视图已重置', { exact: true })).toBeVisible();
  await expect.poll(() => dashboardRequests).toBeGreaterThan(beforeReset);

  await usagePage.getByRole('button', { name: '语言' }).click();
  await expect(usagePage.getByRole('heading', { name: 'Usage' })).toBeVisible();
  await expect(usagePage.getByLabel('Window')).toBeVisible();
  await usagePage.close();

  const failurePage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(failurePage, 'admin', 'failure');
  await failurePage.goto('/console/usage');
  await expect(failurePage.getByRole('alert')).toContainText('用量数据读取失败');
  await expect(failurePage.getByRole('alert')).toContainText('e2e-usage-failure-1');
  await expect(failurePage.locator('.pool-stat').filter({ hasText: '实际 Token' })).toHaveCount(0);
  await failurePage.close();
});

test('settings save, template, lazy section query, and failure states are explicit', async ({ browser }) => {
  const settingsPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(settingsPage, 'admin', 'interactive');
  let configRequests = 0;
  settingsPage.on('request', (request) => {
    if (request.method() === 'GET' && new URL(request.url()).pathname === '/admin/config') configRequests += 1;
  });
  await settingsPage.goto('/console/settings-v2');
  await expect(settingsPage.locator('[data-page-ready="true"]')).toBeVisible();
  await settingsPage.getByRole('button', { name: /^安全/ }).click();
  const securitySwitch = settingsPage.getByRole('switch', { name: '要求下游 Key' });
  await expect(securitySwitch).not.toBeChecked();
  await securitySwitch.click();

  const beforeSave = configRequests;
  const saveRequest = settingsPage.waitForRequest((request) => request.method() === 'POST' && new URL(request.url()).pathname === '/admin/settings-center');
  await settingsPage.getByRole('button', { name: '保存改动 (1)', exact: true }).click();
  expect((await saveRequest).postDataJSON()).toEqual([{ section: 'config', values: { require_downstream_key: true } }]);
  await expect(settingsPage.getByText('已保存 1 项配置', { exact: true })).toBeVisible();
  await expect.poll(() => configRequests).toBeGreaterThan(beforeSave);

  const templateButton = settingsPage.getByRole('button', { name: '应用全模型推荐配置' });
  await expect(templateButton).toBeVisible();
  const [templateRequest] = await Promise.all([
    settingsPage.waitForRequest((request) => request.method() === 'POST' && new URL(request.url()).pathname === '/admin/settings-center/apply-template'),
    templateButton.click(),
  ]);
  expect(templateRequest.postDataJSON()).toEqual({ template_id: 'optimal-stable-models-v1' });
  await expect(settingsPage.getByText('已应用模板: 全模型稳定推荐配置', { exact: true })).toBeVisible();

  const memoryRequest = settingsPage.waitForRequest((request) => {
    const url = new URL(request.url());
    return request.method() === 'GET' && url.pathname === '/admin/settings-center' && url.searchParams.get('sections') === 'memory';
  });
  await settingsPage.getByRole('tab', { name: '内存管理' }).click();
  await memoryRequest;
  await expect(settingsPage).toHaveURL(/settings-v2\?tab=memory/);
  await settingsPage.close();

  const failurePage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(failurePage, 'admin', 'failure');
  await failurePage.goto('/console/settings-v2');
  await expect(failurePage.getByRole('alert')).toContainText('通用配置读取异常');
  await expect(failurePage.getByRole('alert')).toContainText('e2e-settings-failure-1');
  await expect(failurePage.getByText('要求下游 Key', { exact: true })).toHaveCount(0);
  await failurePage.close();
});

test('thinking and moderation settings validate and save typed payloads', async ({ browser }) => {
  const thinkingPage = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(thinkingPage, 'admin', 'interactive');
  await thinkingPage.goto('/console/thinking');
  await expect(thinkingPage).toHaveURL(/settings-v2\?tab=thinking/);
  await expect(thinkingPage.locator('[data-page-ready="true"]')).toBeVisible();
  const thinkingSave = thinkingPage.getByRole('button', { name: '保存', exact: true });
  await expect(thinkingSave).toBeDisabled();

  await thinkingPage.getByLabel('providers (JSON)').fill('{broken');
  await thinkingSave.click();
  await expect(thinkingPage.getByText('providers: 不是合法 JSON', { exact: true })).toBeVisible();
  await thinkingPage.getByLabel('providers (JSON)').fill('{}');
  const enabledSwitch = thinkingPage.getByRole('switch', { name: 'enabled' });
  await enabledSwitch.click();
  await expect(enabledSwitch).not.toBeChecked();
  await expect(thinkingSave).toBeEnabled();
  const thinkingRequest = thinkingPage.waitForRequest((request) => request.method() === 'POST' && new URL(request.url()).pathname === '/admin/thinking');
  await thinkingSave.click();
  expect((await thinkingRequest).postDataJSON()).toEqual({
    enabled: false, default_mode: 'level', default_level: 'medium', default_budget: 4096, providers: {}, models: {},
  });
  await expect(thinkingPage.getByText('高级配置已保存', { exact: true })).toBeVisible();
  await thinkingPage.close();

  const moderationPage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(moderationPage, 'admin', 'interactive');
  await moderationPage.goto('/console/moderation');
  await expect(moderationPage).toHaveURL(/settings-v2\?tab=moderation/);
  await expect(moderationPage.locator('[data-page-ready="true"]')).toBeVisible();
  await moderationPage.getByLabel(/每行一项/).fill('secret\npassword');
  const moderationRequest = moderationPage.waitForRequest((request) => request.method() === 'POST' && new URL(request.url()).pathname === '/admin/moderation');
  await moderationPage.getByRole('button', { name: '保存', exact: true }).click();
  expect((await moderationRequest).postDataJSON()).toEqual({
    enabled: false, model: 'gpt-5-mini', auto_translate: true, words: ['secret', 'password'],
  });
  await expect(moderationPage.getByText('高级配置已保存', { exact: true })).toBeVisible();
  await moderationPage.close();

  const failurePage = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(failurePage, 'admin', 'failure');
  await failurePage.goto('/console/thinking');
  await expect(failurePage.getByRole('alert')).toContainText('高级配置读取失败');
  await expect(failurePage.getByRole('alert')).toContainText('e2e-advanced-settings-failure-1');
  await expect(failurePage.getByLabel('default_budget')).toHaveCount(0);
  await failurePage.close();
});

test('dashboard and portal usage distinguish primary and partial failures', async ({ browser }) => {
  const dashboardPartial = await browser.newPage({ viewport: { width: 1440, height: 900 } });
  await mockBackend(dashboardPartial, 'admin', 'partial');
  await dashboardPartial.goto('/console/');
  await expect(dashboardPartial.getByRole('alert')).toHaveCount(0);
  await expect(dashboardPartial.getByText('可调度账号', { exact: true })).toBeVisible();
  await expect(dashboardPartial.locator('.pool-dashboard-command__metric strong')).toHaveText('2');
  await dashboardPartial.close();

  const dashboardFailure = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(dashboardFailure, 'admin', 'failure');
  await dashboardFailure.goto('/console/');
  await expect(dashboardFailure.getByRole('alert')).toContainText('总览数据读取失败');
  await expect(dashboardFailure.getByRole('alert')).toContainText('e2e-failure-1');
  await expect(dashboardFailure.getByText('可调度账号', { exact: true })).toHaveCount(0);
  await dashboardFailure.close();

  const portalPartial = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(portalPartial, 'user', 'partial');
  await portalPartial.goto('/console/portal');
  await expect(portalPartial.getByRole('alert')).toContainText('趋势数据暂时不可用');
  await expect(portalPartial.locator('.pool-stat').filter({ hasText: '总 Token' }).locator('.value')).toHaveText('1500');
  await expect.poll(() => portalPartial.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await portalPartial.close();

  const portalFailure = await browser.newPage({ viewport: { width: 390, height: 844 } });
  await mockBackend(portalFailure, 'user', 'failure');
  await portalFailure.goto('/console/portal');
  await expect(portalFailure.getByRole('alert')).toContainText('用量数据读取失败');
  await expect(portalFailure.getByRole('alert')).toContainText('e2e-portal-usage-failure-1');
  await expect(portalFailure.getByText('总 Token', { exact: true })).toHaveCount(0);
  await portalFailure.close();
});
