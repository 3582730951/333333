import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import process from 'node:process';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import puppeteer from 'puppeteer';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workspaceRoot = path.resolve(root, '..');
const outDir = path.join(workspaceRoot, '.run', 'ui-review');
const screenshotRoot = path.join(outDir, 'screenshots');
const downloadRoot = path.join(outDir, 'downloads');
const userDataDir = path.join(outDir, 'chrome-profile');
const viteBin = path.join(root, 'node_modules', 'vite', 'bin', 'vite.js');
const serverReadyPattern = /Local:\s+http:\/\/127\.0\.0\.1:/;
const seed = 'ui-review-v6.1-fixed-seed';

const viewports = [
  { name: '1440x900', width: 1440, height: 900 },
  { name: '1280x720', width: 1280, height: 720 },
  { name: '390x844', width: 390, height: 844, mobile: true },
  { name: '360x800', width: 360, height: 800, mobile: true },
];
const themes = ['light', 'dark'];
const adminPages = [
  ['Dashboard', '/'],
  ['Accounts', '/accounts'],
  ['Usage', '/usage'],
  ['Audit', '/audit'],
  ['Settings', '/settings-v2'],
  ['Keys', '/keys'],
  ['System', '/system'],
  ['Registration', '/registration'],
  ['Egress', '/egress'],
];
const userPages = [
  ['PortalDashboard', '/portal'],
  ['PortalKeys', '/portal/keys'],
  ['PortalProfile', '/portal/profile'],
];

const now = 1783125600;
const dayStart = now - (now % 86400);
const fixtures = {
  seed,
  timezoneScenarios: [
    { label: 'vps-new-york', timezone: 'America/New_York', utc_offset_seconds: -14400 },
    { label: 'vps-tokyo', timezone: 'Asia/Tokyo', utc_offset_seconds: 32400 },
    { label: 'vps-utc', timezone: 'UTC', utc_offset_seconds: 0 },
  ],
  notes: [
    'Long account names, long emails, empty fields, large token counts, unusual statuses, low hit/high spend rows, VPS offset, permission denied, network failure, empty data, loading delay and export failure fixtures are deterministic.',
  ],
  accounts: [
    {
      id: 'acct_prod_primary_with_a_very_long_identifier_001',
      label: 'primary-prod-account-with-long-human-label',
      email: 'primary.operator.with.long.email.alias@example-very-long-domain.test',
      provider: 'codex',
      group_name: 'cyber',
      plan_type: 'pro',
      status: 'active',
      usage: { requests: 987654, total_tokens: 987654321, cached_tokens: 456789123 },
    },
    {
      id: 'acct_low_hit_high_spend_002',
      label: '',
      email: '',
      provider: 'claude',
      group_name: 'staging',
      plan_type: 'team',
      status: 'permission_denied',
      quarantine_until: now + 3600,
      usage: { requests: 42, total_tokens: 18000000, cached_tokens: 125 },
    },
    {
      id: 'acct_empty_state_003',
      label: '空值 / 异常状态',
      email: 'empty@example.test',
      provider: 'custom',
      group_name: '',
      plan_type: '',
      status: 'unreachable',
      usage: null,
    },
  ],
  apiKeys: [
    {
      label: 'production-cli-key-with-long-label',
      group_name: 'cyber',
      force_model: 'gpt-5.5',
      force_effort: 'high',
      secret: 'cap_fixture_secret_admin',
      enabled: true,
      key_hash: 'abcdef1234567890abcdef',
      created_at: now - 3600,
    },
    {
      label: 'disabled-low-cache-key',
      group_name: 'staging',
      force_model: '',
      force_effort: '',
      secret: '',
      enabled: false,
      key_hash: 'deadbeef9876543210',
      created_at: now - 86400,
    },
  ],
  usageRows: [
    { account_id: 'acct_prod_primary_with_a_very_long_identifier_001', requests: 1234, prompt_tokens: 7100000, completion_tokens: 890000, total_tokens: 7990000, cached_tokens: 5900000, cache_read_tokens: 5900000, cache_creation_tokens: 320000 },
    { account_id: 'acct_low_hit_high_spend_002', requests: 98, prompt_tokens: 4200000, completion_tokens: 180000, total_tokens: 4380000, cached_tokens: 1200, cache_read_tokens: 1200, cache_creation_tokens: 2100000 },
  ],
  auditRows: [
    { id: 1, created_at: now - 90, account_id: 'acct_prod_primary_with_a_very_long_identifier_001', account_label: 'primary-prod-account-with-long-human-label', action: 'usage_daily_window_reset', state: 'ok', reason: 'daily_window', detail: 'VPS local day reset' },
    { id: 2, created_at: now - 30, account_id: 'acct_low_hit_high_spend_002', account_label: '', action: 'usage_cache_stats_reset', state: 'ok', reason: 'manual_reset', detail: 'Manual cache diagnostics baseline advanced without deleting history' },
    { id: 3, created_at: now - 10, account_id: 'acct_empty_state_003', account_label: '空值 / 异常状态', action: 'permission_denied_no_quarantine', state: 'permission_denied', reason: 'scope', detail: 'Long diagnostic detail that should ellipsize in table cells without forcing page overflow' },
  ],
  users: [
    { id: 'user_admin', email: 'admin@example.test', name: 'Admin Fixture', role: 'admin', status: 'active', created_at: now - 86400 },
    { id: 'user_normal', email: 'user@example.test', name: 'User Fixture With Long Display Name', role: 'user', status: 'active', created_at: now - 3600 },
  ],
  configFields: [
    { key: 'require_downstream_key', label: '下游 Key 必填', category: '访问控制', type: 'bool', effect: 'hot', options: [], help: '要求普通请求携带用户 API Key。', value: true, overridden: true },
    { key: 'conversation_isolation', label: '会话隔离', category: '访问控制', type: 'bool', effect: 'hot', options: [], help: '按账号隔离上游会话标识。', value: true, overridden: false },
    { key: 'claude_cache_ttl', label: 'Claude 缓存 TTL', category: '缓存', type: 'select', effect: 'hot', options: ['', '5m', '1h'], help: 'Claude prompt cache control TTL。', value: '1h', overridden: true },
  ],
};

const operationLog = [];
function log(event, detail = {}) {
  operationLog.push({ at: new Date().toISOString(), event, ...detail });
}

function mkdirs() {
  fs.rmSync(outDir, { recursive: true, force: true });
  fs.mkdirSync(screenshotRoot, { recursive: true });
  fs.mkdirSync(downloadRoot, { recursive: true });
  fs.mkdirSync(userDataDir, { recursive: true });
  fs.copyFileSync(fileURLToPath(import.meta.url), path.join(outDir, 'browser-script.mjs'));
  fs.writeFileSync(path.join(outDir, 'fixtures.json'), `${JSON.stringify(fixtures, null, 2)}\n`);
}

function canUsePort(port) {
  return new Promise((resolve) => {
    const server = net.createServer();
    server.once('error', () => resolve(false));
    server.once('listening', () => server.close(() => resolve(true)));
    server.listen(port, '127.0.0.1');
  });
}

async function findPort(start) {
  for (let port = start; port < start + 40; port += 1) {
    if (await canUsePort(port)) return port;
  }
  throw new Error(`No free port found from ${start}`);
}

function waitForServer(child) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const cleanup = () => {
      child.stdout.off('data', onData);
      child.stderr.off('data', onData);
      child.off('exit', onExit);
      clearTimeout(timeout);
    };
    const finish = (fn, value) => {
      if (settled) return;
      settled = true;
      cleanup();
      fn(value);
    };
    const timeout = setTimeout(() => finish(reject, new Error('Vite server did not become ready in time')), 30000);
    const onData = (chunk) => {
      const text = String(chunk);
      if (serverReadyPattern.test(text)) finish(resolve);
    };
    const onExit = (code) => finish(reject, new Error(`Vite server exited before ready: ${code}`));
    child.stdout.on('data', onData);
    child.stderr.on('data', onData);
    child.on('exit', onExit);
  });
}

function startServer(port) {
  return spawn(process.execPath, [viteBin, '--host', '127.0.0.1', '--port', String(port), '--strictPort'], {
    cwd: root,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
}

async function stopServer(child) {
  if (!child || child.exitCode !== null || child.signalCode) return;
  await new Promise((resolve) => {
    const force = setTimeout(() => {
      if (child.exitCode === null && !child.signalCode) child.kill('SIGKILL');
    }, 3000);
    child.once('exit', () => {
      clearTimeout(force);
      resolve();
    });
    child.kill('SIGTERM');
  });
}

function json(body, status = 200, headers = {}) {
  return { status, contentType: 'application/json', headers, body: JSON.stringify(body) };
}

function currentRole(req) {
  const cookie = req.headers().cookie || '';
  if (cookie.includes('cp_session=user-fixture')) return 'user';
  if (cookie.includes('cp_session=admin-fixture')) return 'admin';
  return '';
}

function fixtureParams(req) {
  const params = new URLSearchParams();
  const merge = (raw) => {
    if (!raw) return;
    try {
      const parsed = new URL(raw);
      for (const [key, value] of parsed.searchParams.entries()) params.set(key, value);
    } catch {
      // Ignore malformed fixture referrers from browser internals.
    }
  };
  merge(req.url());
  merge(req.headers().referer || '');
  return params;
}

function hasFixture(req, name) {
  return fixtureParams(req).get(name) === '1';
}

function delay(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

function usageWindow(req) {
  const tz = fixtureParams(req).get('fixture_tz');
  const timezone = tz === 'tokyo' ? 'Asia/Tokyo' : tz === 'utc' ? 'UTC' : 'America/New_York';
  const utcOffset = tz === 'tokyo' ? 32400 : tz === 'utc' ? 0 : -14400;
  return {
    window: {
      timezone,
      utc_offset_seconds: utcOffset,
      day_start_at: dayStart,
      next_day_start_at: dayStart + 86400,
      now_at: now,
      cache_stats_reset_at: now - 1800,
    },
    window_mode: 'default',
    effective_start_at: dayStart,
    effective_until_at: now,
  };
}

function cacheReport(req) {
  return {
    ...usageWindow(req),
    effective_start_at: now - 1800,
    summary: {
      requests: 1332,
      real_requests: 1300,
      hit_requests: 812,
      request_hit_rate: 0.61,
      prompt_tokens: 11300000,
      cached_tokens: 5901200,
      cache_input_tokens: 13500000,
      cache_miss_tokens: 7598800,
      cache_read_tokens: 5901200,
      cache_creation_tokens: 2420000,
      token_hit_rate: 0.437,
      cache_read_share: 0.437,
      cache_write_share: 0.179,
      eligible_cache_hit_rate: 0.709,
      real_token_hit_rate: 0.441,
      estimated_requests: 32,
      estimated_rate: 0.024,
    },
    by_api_key: [
      { api_key_hash_prefix: 'abcdef123456', requests: 1200, hit_requests: 790, request_hit_rate: 0.66, token_hit_rate: 0.52, real_token_hit_rate: 0.53, cache_read_tokens: 5900000, cache_creation_tokens: 320000, cache_miss_tokens: 1200000, estimated_rate: 0.01 },
      { api_key_hash_prefix: 'deadbeef9876', requests: 132, hit_requests: 22, request_hit_rate: 0.17, token_hit_rate: 0.01, real_token_hit_rate: 0.01, cache_read_tokens: 1200, cache_creation_tokens: 2100000, cache_miss_tokens: 4200000, estimated_rate: 0.13 },
    ],
    by_account_model: [
      { account_id: fixtures.accounts[0].id, model: 'gpt-5.5', requests: 1234, hit_requests: 800, request_hit_rate: 0.65, token_hit_rate: 0.52, real_token_hit_rate: 0.52, cache_read_tokens: 5900000, cache_creation_tokens: 320000, cache_miss_tokens: 1200000 },
      { account_id: fixtures.accounts[1].id, model: 'claude-sonnet-4', requests: 98, hit_requests: 12, request_hit_rate: 0.12, token_hit_rate: 0.01, real_token_hit_rate: 0.01, cache_read_tokens: 1200, cache_creation_tokens: 2100000, cache_miss_tokens: 4200000 },
    ],
    by_route: [
      { route_key_hash_prefix: 'routeabc12345', affinity_source: 'prompt_cache_key', prompt_cache_key_source: 'client', stable_prefix_source: 'messages', stable_prefix_reason: 'prefix_match', stable_prefix_bytes: 120000, retention_effective: '24h', retention_source: 'server', claude_cache_ttl: '1h', requests: 900, request_hit_rate: 0.72, real_token_hit_rate: 0.56, cache_read_tokens: 4100000, cache_creation_tokens: 220000, cache_miss_tokens: 900000, cache_breakpoint_count: 3, latest_user_cache_control: 0 },
    ],
    by_route_account_model: [
      { route_key_hash_prefix: 'routeabc12345', account_id: fixtures.accounts[0].id, model: 'gpt-5.5', affinity_source: 'prompt_cache_key', requests: 900, request_hit_rate: 0.72, real_token_hit_rate: 0.56, cache_read_tokens: 4100000, cache_creation_tokens: 220000, cache_miss_tokens: 900000 },
    ],
    by_time_bucket: [
      { bucket: now - 7200, requests: 120, real_requests: 119, cache_read_share: 0.3, cache_write_share: 0.2, cache_read_tokens: 120000, cache_creation_tokens: 80000, cache_miss_tokens: 300000, estimated_rate: 0.01 },
      { bucket: now - 3600, requests: 260, real_requests: 250, cache_read_share: 0.58, cache_write_share: 0.12, cache_read_tokens: 540000, cache_creation_tokens: 95000, cache_miss_tokens: 220000, estimated_rate: 0.03 },
    ],
  };
}

function timeseries(req) {
  return {
    ...usageWindow(req),
    since: dayStart,
    now,
    bucket: 3600,
    buckets: Array.from({ length: 8 }, (_, i) => ({
      bucket: now - (7 - i) * 3600,
      requests: 60 + i * 12,
      prompt_tokens: 120000 + i * 15000,
      completion_tokens: 22000 + i * 4000,
      cached_tokens: 80000 + i * 12000,
      cache_read_tokens: 80000 + i * 12000,
      cache_creation_tokens: 9000 + i * 1000,
      total_tokens: 142000 + i * 19000,
    })),
  };
}

async function handleAPI(req) {
  const url = new URL(req.url());
  const p = url.pathname;
  const role = currentRole(req);
  if (p === '/healthz') return req.respond(json({ ok: true }));
  if (p === '/client/errors') return req.respond({ status: 204, body: '' });
  if (p === '/auth/login') {
    const email = JSON.parse(req.postData() || '{}').email || '';
    const nextRole = email.includes('user') ? 'user' : 'admin';
    return req.respond(json({
      id: nextRole === 'admin' ? 'user_admin' : 'user_normal',
      email,
      name: nextRole === 'admin' ? 'Admin Fixture' : 'User Fixture',
      role: nextRole,
      status: 'active',
    }, 200, {
      'set-cookie': `cp_session=${nextRole}-fixture; Max-Age=86400; Path=/; SameSite=Lax`,
    }));
  }
  if (p === '/auth/me') {
    if (!role) return req.respond(json({ authed: false }, 401));
    return req.respond(json({
      authed: true,
      via: 'session',
      id: role === 'admin' ? 'user_admin' : 'user_normal',
      role,
      email: role === 'admin' ? 'admin@example.test' : 'user@example.test',
      name: role === 'admin' ? 'Admin Fixture' : 'User Fixture',
    }));
  }
  if (p === '/admin/config' && hasFixture(req, 'fixture_login_delay')) {
    await delay(900);
    return req.respond(json({ error: { message: 'fixture invalid admin token' } }, 403));
  }
  if (p.startsWith('/admin/') && role !== 'admin') {
    return req.respond(json({ error: { message: 'admin role required' } }, 403));
  }
  if (p === '/admin/config') return req.respond(json(fixtures.configFields));
  if (p === '/admin/settings') return req.respond(json({ conversation_isolation: true, claude_cache_control_inject: true, require_downstream_key: true }));
  if (p === '/admin/settings-center') return req.respond(json({
    config: { values: { require_downstream_key: true, web_search_enabled: true, claude_cache_ttl: '1h' } },
    registrar: { values: { default_register_method: 'protocol', registration_concurrency: 2 } },
    automation: { policy: { enabled: true, type: 'refill', config: { target: 8, threshold: 2 } } },
    logging: { values: {} },
    memory: { values: {} },
  }));
  if (p === '/admin/accounts' || p === '/admin/accounts/summary') return req.respond(json({ accounts: fixtures.accounts, rows: fixtures.accounts, total: fixtures.accounts.length }));
  if (p === '/admin/groups') return req.respond(json({ groups: [{ name: 'cyber', force_model: 'gpt-5.5', force_effort: 'high' }, { name: 'staging', force_model: '', force_effort: '' }] }));
  if (p === '/admin/user-groups') return req.respond(json([{ id: 'ug_fixture', name: 'Fixture users', targets: [{ kind: 'account_pool_group', id: 'cyber' }] }]));
  if (p === '/admin/api-keys') return req.respond(json({ keys: fixtures.apiKeys }));
  if (p === '/admin/users') return req.respond(json({ users: fixtures.users }));
  if (p === '/admin/providers' || p === '/admin/register/providers') return req.respond(json({ providers: [{ id: 'openai', name: 'OpenAI', base_url: 'https://api.openai.com/v1', enabled: true, auto_discover_models: true, models: ['gpt-5.5', 'gpt-5.4'] }] }));
  if (p === '/admin/egress-profiles') return req.respond(json({ profiles: [{ id: 'egress_direct', name: 'Direct', type: 'direct', region: 'us-east', health: 'ok', latency_millis: 42, stream_capable: true }] }));
  if (p === '/admin/egress-pools') return req.respond(json({ pools: [{ id: 'pool_registration', name: 'Registration Pool', purpose: 'registration', members: ['egress_direct'] }, { id: 'pool_runtime', name: 'Runtime Pool', purpose: 'runtime', members: ['egress_direct'] }] }));
  if (hasFixture(req, 'force403') && p.startsWith('/admin/usage')) return req.respond(json({ error: { message: 'admin role required' } }, 403));
  if (p === '/admin/usage/dashboard') {
    const trend = timeseries(req);
    return req.respond(json({
      ...usageWindow(req),
      accounts: fixtures.usageRows,
      timeseries: trend.buckets,
      models: cacheReport(req).by_provider_model,
      model_series: trend.model_series,
      series: trend.series,
      cache: cacheReport(req),
    }));
  }
  if (p === '/admin/usage') return req.respond(json({ ...usageWindow(req), rows: fixtures.usageRows }));
  if (p === '/admin/usage/window') return req.respond(json({ ...usageWindow(req), cache_effective_start_at: now - 1800 }));
  if (p === '/admin/usage/timeseries') return req.respond(json(timeseries(req)));
  if (p === '/admin/usage/by-model') return req.respond(json({ ...usageWindow(req), models: [{ model: 'gpt-5.5', requests: 1234, prompt_tokens: 7100000, completion_tokens: 890000, total_tokens: 7990000, cached_tokens: 5900000, cache_input_tokens: 10300000, cache_read_tokens: 5900000, cache_creation_tokens: 320000 }, { model: 'claude-sonnet-4', requests: 98, prompt_tokens: 4200000, completion_tokens: 180000, total_tokens: 4380000, cached_tokens: 1200, cache_input_tokens: 6300000, cache_read_tokens: 1200, cache_creation_tokens: 2100000 }] }));
  if (p === '/admin/usage/cache') return req.respond(json(cacheReport(req)));
  if (p === '/admin/usage/cache/reset') {
    await delay(700);
    return req.respond(json({ ...usageWindow(req), effective_start_at: now, reset_at: now }));
  }
  if (p === '/admin/audit') {
    if (hasFixture(req, 'fixture_loading') || hasFixture(req, 'fixture_error')) await delay(900);
    if (hasFixture(req, 'fixture_error')) return req.respond(json({ error: { message: 'fixture network failure' } }, 500));
    if (hasFixture(req, 'fixture_empty')) return req.respond(json({ rows: [] }));
    return req.respond(json({ rows: fixtures.auditRows }));
  }
  if (p === '/admin/quota') return req.respond(json([{ account_id: fixtures.accounts[0].id, provider: 'codex', model: 'gpt-5.5', used_percent: 62, remaining_tokens: 123456, limit_tokens: 999999, status: 'ok' }]));
  if (p === '/admin/system') return req.respond(json({
    supported: true,
    hostname: 'fixture-vps',
    uptime_seconds: 456789,
    cpu: { usage_pct: 18, cores: 4, load1: 0.42, load5: 0.55, load15: 0.61 },
    mem: { total_kb: 8388608, used_kb: 4128768, used_pct: 49 },
    disk: { path: '/', total_bytes: 68719476736, used_bytes: 21474836480, free_bytes: 47244640256, used_pct: 31 },
    registration: {
      total_rss_kb: 196608,
      node: 1,
      chrome: 2,
      xvfb: 1,
      procs: [
        { pid: 4101, comm: 'node', kind: 'node', rss_kb: 65536 },
        { pid: 4108, comm: 'chrome', kind: 'chrome', rss_kb: 98304 },
      ],
    },
    go: { goroutines: 42, sys_bytes: 134217728 },
    supervisor_modules: [
      { name: 'registrar-worker', status: 'running', restart_count: 0, panic_count: 0, unexpected_exit_count: 0, uptime_millis: 456000, last_message: 'healthy' },
      { name: 'cache-diagnostics', status: 'running', restart_count: 1, panic_count: 0, unexpected_exit_count: 0, uptime_millis: 256000, last_message: 'healthy' },
    ],
    supervisor_events: [
      { time_unix: now - 300, module: 'cache-diagnostics', type: 'event', message: 'fixture heartbeat', uptime_millis: 256000, backoff_millis: 0 },
    ],
  }));
  if (p === '/admin/cf-events') return req.respond(json([{ id: 1, created_at: now - 60, account_id: fixtures.accounts[0].id, status: 403, cf_ray: 'fixture-ray', category: 'challenge', message: 'fixture challenge' }]));
  if (p === '/admin/register/readiness') return req.respond(json({ ready: true, blockers: [], warnings: ['低命中高消耗 fixture'] }));
  if (p === '/admin/register/stats' || p === '/admin/automation/stats') return req.respond(json({ pending: 1, running: 0, succeeded: 7, failed: 1, last_error: '' }));
  if (p === '/admin/register/countries') return req.respond(json({ countries: [{ code: 'US', name: 'United States' }, { code: 'JP', name: 'Japan' }] }));
  if (p === '/admin/export/cache-hits') {
    await delay(650);
    if (hasFixture(req, 'fixture_export_fail')) return req.respond(json({ error: { message: 'fixture export failure' } }, 500));
    return req.respond({ status: 200, headers: { 'content-type': 'application/zip', 'content-disposition': 'attachment; filename="codex-pool-cache-hits-fixture.zip"' }, body: Buffer.from('cache-hit-zip-fixture') });
  }
  if (p === '/admin/export/logs') return req.respond({ status: 200, headers: { 'content-type': 'application/zip', 'content-disposition': 'attachment; filename="codex-pool-diagnostics-fixture.zip"' }, body: Buffer.from('diagnostics-zip-fixture') });
  if (p === '/user/usage') return req.respond(json([{ model: 'gpt-5.5', requests: 18, prompt_tokens: 12000, completion_tokens: 800, total_tokens: 12800, cached_tokens: 4200, cache_input_tokens: 12000, cache_read_tokens: 4200, cache_creation_tokens: 700 }]));
  if (p === '/user/usage/timeseries') return req.respond(json({ buckets: timeseries(req).buckets.slice(0, 5) }));
  if (p === '/user/api-keys') return req.respond(json([{ label: 'my-portal-key-with-long-label', key_hash: 'portal1234567890abcdef', enabled: true, created_at: now - 1200, secret: 'cap_user_fixture' }]));
  if (p === '/user/profile') return req.respond(json({ email: 'user@example.test', name: 'User Fixture With Long Display Name', role: 'user' }));
  if (p.startsWith('/admin/') || p.startsWith('/user/')) return req.respond(json({}));
  return req.continue();
}

async function installMocks(page) {
  await page.setRequestInterception(true);
  page.on('request', async (req) => {
    try {
      await handleAPI(req);
    } catch (error) {
      log('mock_error', { url: req.url(), message: error.message });
      await req.respond(json({ error: { message: error.message } }, 500));
    }
  });
}

async function preparePage(browser, baseURL, role, viewport, theme) {
  const page = await browser.newPage();
  const client = await page.target().createCDPSession();
  await client.send('Page.setDownloadBehavior', { behavior: 'allow', downloadPath: downloadRoot });
  await installMocks(page);
  await page.setViewport({
    width: viewport.width,
    height: viewport.height,
    deviceScaleFactor: viewport.mobile ? 2 : 1,
    isMobile: !!viewport.mobile,
  });
  await page.emulateMediaFeatures([
    { name: 'prefers-color-scheme', value: theme },
    { name: 'prefers-reduced-motion', value: 'reduce' },
  ]);
  await page.setCookie(
    { url: baseURL, name: 'cp_session', value: `${role}-fixture`, path: '/', expires: Math.floor(Date.now() / 1000) + 86400 },
    { url: baseURL, name: 'cp_csrf', value: `${role}-csrf`, path: '/', expires: Math.floor(Date.now() / 1000) + 86400 },
  );
  await page.evaluateOnNewDocument((nextTheme) => {
    localStorage.setItem('pool_theme', nextTheme);
    document.documentElement.setAttribute('data-theme', nextTheme);
  }, theme);
  page.on('response', (response) => {
    if (response.status() >= 400 && !response.url().includes('/auth/me')) {
      const req = response.request();
      const expected = hasFixture(req, 'force403') || hasFixture(req, 'fixture_error') || hasFixture(req, 'fixture_export_fail') || hasFixture(req, 'fixture_login_delay');
      log(expected ? 'expected_http_error' : 'http_error', { status: response.status(), url: response.url() });
    }
  });
  return page;
}

async function gotoApp(page, baseURL, route) {
  await page.goto(`${baseURL}${route}`, { waitUntil: 'domcontentloaded', timeout: 45000 });
  await page.waitForFunction(() => document.querySelector('#root') && document.body.innerText.trim().length > 10, { timeout: 45000 });
  await new Promise((resolve) => setTimeout(resolve, 350));
}

async function pageMetrics(page) {
  return page.evaluate(() => {
    const textOverflows = [];
    document.querySelectorAll('.pool-table td, .pool-table th').forEach((cell) => {
      const cellRect = cell.getBoundingClientRect();
      if (cellRect.width <= 0 || cellRect.height <= 0) return;
      const walker = document.createTreeWalker(cell, NodeFilter.SHOW_TEXT);
      while (walker.nextNode()) {
        const node = walker.currentNode;
        const text = (node.nodeValue || '').replace(/\s+/g, ' ').trim();
        if (!text) continue;
        const range = document.createRange();
        range.selectNodeContents(node);
        const rects = [...range.getClientRects()].filter((rect) => rect.width > 0 && rect.height > 0);
        range.detach();
        if (rects.some((rect) => rect.right > cellRect.right + 1 || rect.left < cellRect.left - 1)) {
          const column = cell.getAttribute('data-label') || cell.closest('table')?.querySelectorAll('th')?.[cell.cellIndex]?.innerText || '';
          textOverflows.push({
            column: column.replace(/\s+/g, ' ').trim().slice(0, 40),
            text: text.slice(0, 80),
            cellWidth: Math.round(cellRect.width),
            maxTextRight: Math.round(Math.max(...rects.map((rect) => rect.right))),
            cellRight: Math.round(cellRect.right),
          });
        }
      }
    });
    const blankChartCards = [...document.querySelectorAll('.pool-chart-card')].map((card) => {
      const rect = card.getBoundingClientRect();
      if (rect.width <= 0 || rect.height <= 120) return null;
      const text = (card.innerText || '').replace(/\s+/g, ' ').trim();
      const title = card.querySelector('.t')?.textContent?.trim() || text.slice(0, 40);
      if (/暂无数据|加载图表/.test(text)) return null;
      const visibleVisuals = [...card.querySelectorAll('svg path, svg rect, svg circle, .pool-meter span, .pool-cache-breakdown__bar span')]
        .filter((el) => {
          const style = getComputedStyle(el);
          const box = el.getBoundingClientRect();
          return style.display !== 'none' && style.visibility !== 'hidden' && box.width > 1 && box.height > 1;
        });
      if (visibleVisuals.length > 0) return null;
      return {
        title,
        height: Math.round(rect.height),
        text: text.slice(0, 80),
      };
    }).filter(Boolean);
    return {
      textOverflows: textOverflows.slice(0, 12),
      blankChartCards: blankChartCards.slice(0, 8),
      path: location.pathname + location.search,
      textLength: document.body.innerText.length,
      documentWidth: document.documentElement.scrollWidth,
      viewportWidth: window.innerWidth,
      noPageOverflow: document.documentElement.scrollWidth <= window.innerWidth + 1,
      focusedVisible: !!document.querySelector(':focus-visible'),
    };
  });
}

function assertNoTextOverlap(metrics, label) {
  if (metrics.textOverflows?.length) {
    throw new Error(`${label} has table text overflow: ${JSON.stringify(metrics.textOverflows.slice(0, 4))}`);
  }
  if (metrics.blankChartCards?.length) {
    throw new Error(`${label} has blank chart card: ${JSON.stringify(metrics.blankChartCards.slice(0, 3))}`);
  }
}

async function assertPageContent(page, name, label) {
  const requirements = {
    Dashboard: ['总览', '账号总数', 'Token 用量'],
    Usage: ['用量分析', 'Top 账号', '按 API Key 缓存诊断'],
    Settings: ['设置中心', '通用配置', '下游 Key 必填'],
    Registration: ['自动注册', '启动注册任务', '暂无注册任务'],
    System: ['系统监控', 'CPU 使用率', '资源与模块状态'],
    PortalDashboard: ['我的用量', '总 Token', '按模型用量'],
    PortalKeys: ['我的 API Key', '复制 Key'],
    PortalProfile: ['我的资料', 'user@example.test'],
  };
  const required = requirements[name];
  if (!required) return;
  await page.waitForFunction((items) => {
    const text = document.body.innerText;
    return items.every((item) => text.includes(item));
  }, { timeout: 45000 }, required);
  const text = await page.evaluate(() => document.body.innerText);
  const missing = required.filter((item) => !text.includes(item));
  if (missing.length) {
    throw new Error(`${label} is missing expected content: ${missing.join(', ')}`);
  }
  if (/\b(undefined|NaN)\b/.test(text)) {
    throw new Error(`${label} contains invalid rendered value`);
  }
}

async function capturePage(browser, baseURL, role, theme, viewport, name, route) {
  const page = await preparePage(browser, baseURL, role, viewport, theme);
  const dir = path.join(screenshotRoot, role, theme, viewport.name);
  fs.mkdirSync(dir, { recursive: true });
  const file = path.join(dir, `${name}.png`);
  await gotoApp(page, baseURL, route);
  await assertPageContent(page, name, `${role}/${theme}/${viewport.name}/${name}`);
  await page.screenshot({ path: file, fullPage: true });
  const metrics = await pageMetrics(page);
  assertNoTextOverlap(metrics, `${role}/${theme}/${viewport.name}/${name}`);
  log('screenshot', { role, theme, viewport: viewport.name, page: name, route, file: path.relative(workspaceRoot, file), metrics });
  await page.close();
}

async function launchReviewBrowser() {
  return puppeteer.launch({
    headless: 'new',
    userDataDir,
    args: ['--no-sandbox', '--disable-dev-shm-usage'],
  });
}

async function loginAndVerifyReuse(baseURL) {
  const firstBrowser = await launchReviewBrowser();
  const page = await firstBrowser.newPage();
  try {
    await installMocks(page);
    await page.setViewport({ width: 1280, height: 720 });
    await page.goto(`${baseURL}/usage`, { waitUntil: 'domcontentloaded' });
    await page.waitForSelector('body', { timeout: 30000 });
    await page.waitForFunction(() => document.body.innerText.includes('用户登录') || document.body.innerText.includes('Pool 控制台'), { timeout: 30000 });
    await page.evaluate(() => localStorage.setItem('pool_theme', 'light'));
    await page.evaluate(async () => {
      const res = await fetch('/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email: 'admin@example.test', password: 'fixture-password' }),
      });
      if (!res.ok) throw new Error(`fixture login failed: ${res.status}`);
    });
    const expires = Math.floor(Date.now() / 1000) + 86400;
    await page.evaluate(() => {
      document.cookie = 'cp_session=admin-fixture; Max-Age=86400; Path=/; SameSite=Lax';
      document.cookie = 'cp_csrf=admin-csrf; Max-Age=86400; Path=/; SameSite=Lax';
    });
    await page.setCookie(
      { url: baseURL, name: 'cp_session', value: 'admin-fixture', path: '/', sameSite: 'Lax', expires },
      { url: baseURL, name: 'cp_csrf', value: 'admin-csrf', path: '/', sameSite: 'Lax', expires },
    );
    const cookies = await page.cookies(baseURL);
    log('login_cookies', { cookies: cookies.map((cookie) => ({ name: cookie.name, value: cookie.value, expires: cookie.expires, session: cookie.session })) });
    await gotoApp(page, baseURL, '/usage');
    log('login', { role: 'admin', method: 'fixture-mocked-login' });
  } finally {
    await page.close().catch(() => {});
    await firstBrowser.close();
  }

  const browser = await launchReviewBrowser();
  const reused = await browser.newPage();
  try {
    await installMocks(reused);
    await reused.setViewport({ width: 1280, height: 720 });
    await gotoApp(reused, baseURL, '/usage');
    await reused.waitForFunction(() => document.body.innerText.includes('管理控制台') && !document.body.innerText.includes('管理员 Token'), { timeout: 45000 });
    const text = await reused.evaluate(() => document.body.innerText);
    const cookies = await reused.cookies(baseURL);
    const sessionReused = cookies.some((cookie) => cookie.name === 'cp_session' && cookie.value === 'admin-fixture') && text.includes('管理控制台') && !text.includes('管理员 Token');
    log('session_reuse_check', {
      session_reused: sessionReused,
      browser_restarted: true,
      cookies: cookies.map((cookie) => ({ name: cookie.name, value: cookie.value, expires: cookie.expires, session: cookie.session })),
      body_excerpt: text.slice(0, 160),
    });
    return { browser, sessionReused };
  } finally {
    await reused.close().catch(() => {});
  }
}

async function buttonHandleByText(page, text) {
  await page.waitForFunction((label) => [...document.querySelectorAll('button')].some((el) => {
    const rect = el.getBoundingClientRect();
    const visible = rect.width > 0 && rect.height > 0;
    return visible && (el.textContent || '').includes(label);
  }), { timeout: 45000 }, text);
  const buttons = await page.$$('button');
  const matches = [];
  for (const button of buttons) {
    const match = await button.evaluate((el, label) => {
      const rect = el.getBoundingClientRect();
      const visible = rect.width > 0 && rect.height > 0;
      const value = (el.textContent || '').trim();
      return { visible, exact: value === label, includes: value.includes(label) };
    }, text);
    if (match.visible && match.exact) return button;
    if (match.visible && match.includes) matches.push(button);
  }
  if (matches[0]) return matches[0];
  throw new Error(`button not found: ${text}`);
}

async function clickButtonByText(page, text) {
  const button = await buttonHandleByText(page, text);
  await button.click();
  return button;
}

async function waitForText(page, text, timeout = 45000) {
  await page.waitForFunction((needle) => document.body.innerText.includes(needle), { timeout }, text);
}

async function captureState(page, dir, filename, states, covered, extra = {}) {
  const file = path.join(dir, filename);
  await page.screenshot({ path: file, fullPage: true });
  const metrics = await pageMetrics(page);
  assertNoTextOverlap(metrics, filename);
  states.forEach((state) => covered.add(state));
  log('state_capture', { states, file: path.relative(workspaceRoot, file), metrics, ...extra });
}

async function waitForNewDownload(before, timeout = 8000) {
  const started = Date.now();
  while (Date.now() - started < timeout) {
    const after = fs.readdirSync(downloadRoot).filter((name) => !before.has(name) && !name.endsWith('.crdownload'));
    if (after.length) return after;
    await delay(100);
  }
  throw new Error('download did not finish in time');
}

async function captureStates(browser, baseURL) {
  const covered = new Set();
  const dir = path.join(screenshotRoot, 'states');
  fs.mkdirSync(dir, { recursive: true });

  const login = await browser.newPage();
  await installMocks(login);
  await login.setViewport({ width: 1280, height: 720 });
  await login.deleteCookie(
    { url: baseURL, name: 'cp_session' },
    { url: baseURL, name: 'cp_csrf' },
  );
  await login.goto(`${baseURL}/?fixture_login_delay=1`, { waitUntil: 'domcontentloaded' });
  await waitForText(login, 'Pool 控制台');
  await clickButtonByText(login, '登录');
  await waitForText(login, '请输入 Token');
  await captureState(login, dir, 'login-validation.png', ['validation'], covered);
  await login.type('input[placeholder="admin_token"]', 'wrong-admin-token');
  await clickButtonByText(login, '登录');
  await delay(150);
  await captureState(login, dir, 'login-submitting.png', ['submitting', 'loading', 'disabled'], covered);
  await waitForText(login, 'Token 无效');
  await captureState(login, dir, 'login-error-toast.png', ['error'], covered);
  await login.close();

  const accounts = await preparePage(browser, baseURL, 'admin', { name: '1440x900', width: 1440, height: 900 }, 'light');
  await gotoApp(accounts, baseURL, '/accounts');
  const checkboxes = await accounts.$$('input[type="checkbox"]');
  if (checkboxes.length < 2) throw new Error('accounts selection checkboxes not found');
  await checkboxes[0].click();
  await checkboxes[1].click();
  await waitForText(accounts, '已选');
  await accounts.waitForFunction(() => document.body.innerText.includes('已选'), { timeout: 45000 });
  await captureState(accounts, dir, 'accounts-bulk-selected.png', ['selected', 'bulk-selected'], covered);
  await accounts.close();

  const page = await preparePage(browser, baseURL, 'admin', { name: '1440x900', width: 1440, height: 900 }, 'light');
  await gotoApp(page, baseURL, '/usage');
  const resetButton = await buttonHandleByText(page, '重置当前缓存统计视图');
  await resetButton.hover();
  await resetButton.focus();
  await captureState(page, dir, 'usage-reset-button-hover-focus.png', ['hover', 'focus'], covered);
  const box = await resetButton.boundingBox();
  if (!box) throw new Error('reset button has no layout box');
  await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
  await page.mouse.down();
  await captureState(page, dir, 'usage-reset-button-pressed.png', ['pressed'], covered);
  await page.mouse.up();
  await resetButton.click();
  await waitForText(page, '不会删除 usage_records');
  await captureState(page, dir, 'usage-reset-modal.png', ['modal/drawer'], covered);
  await clickButtonByText(page, '重置视图');
  await waitForText(page, '缓存统计视图已重置');
  await captureState(page, dir, 'usage-reset-success-toast.png', ['success toast'], covered);
  await page.close();

  const tokyo = await preparePage(browser, baseURL, 'admin', { name: '1280x720', width: 1280, height: 720 }, 'dark');
  await gotoApp(tokyo, baseURL, '/usage?fixture_tz=tokyo');
  await waitForText(tokyo, 'Asia/Tokyo');
  await captureState(tokyo, dir, 'usage-vps-tokyo-offset.png', ['timezone-offset'], covered);
  await tokyo.close();

  const loading = await preparePage(browser, baseURL, 'admin', { name: '1280x720', width: 1280, height: 720 }, 'light');
  await loading.goto(`${baseURL}/audit?fixture_loading=1`, { waitUntil: 'domcontentloaded', timeout: 45000 });
  await loading.waitForSelector('.pool-skel, .pool-spinner, .pool-table-empty', { timeout: 45000 });
  await captureState(loading, dir, 'audit-loading.png', ['loading'], covered);
  await waitForText(loading, 'usage_daily_window_reset');
  await loading.close();

  const empty = await preparePage(browser, baseURL, 'admin', { name: '1280x720', width: 1280, height: 720 }, 'light');
  await gotoApp(empty, baseURL, '/audit?fixture_empty=1');
  await waitForText(empty, '暂无审计记录');
  await captureState(empty, dir, 'audit-empty.png', ['empty'], covered);
  await empty.close();

  const errorPage = await preparePage(browser, baseURL, 'admin', { name: '1280x720', width: 1280, height: 720 }, 'light');
  await gotoApp(errorPage, baseURL, '/audit?fixture_error=1');
  await waitForText(errorPage, 'fixture network failure');
  await captureState(errorPage, dir, 'audit-network-error.png', ['network failure', 'error'], covered);
  await errorPage.close();

  const denied = await preparePage(browser, baseURL, 'admin', { name: '390x844', width: 390, height: 844, mobile: true }, 'light');
  await denied.evaluateOnNewDocument(() => {
    window.addEventListener('pool-unauthorized', (event) => event.stopImmediatePropagation(), true);
  });
  await gotoApp(denied, baseURL, '/usage?force403=1');
  await waitForText(denied, 'admin role required');
  await captureState(denied, dir, 'permission-denied-admin-usage.png', ['permission_denied'], covered);
  await denied.close();

  const failedExport = await preparePage(browser, baseURL, 'admin', { name: '1280x720', width: 1280, height: 720 }, 'dark');
  await gotoApp(failedExport, baseURL, '/audit?fixture_export_fail=1');
  await clickButtonByText(failedExport, '导出缓存命中 ZIP');
  await waitForText(failedExport, '导出缓存命中 ZIP 失败');
  await captureState(failedExport, dir, 'audit-export-failed.png', ['export failure', 'error'], covered);
  await failedExport.close();

  const audit = await preparePage(browser, baseURL, 'admin', { name: '1280x720', width: 1280, height: 720 }, 'dark');
  await gotoApp(audit, baseURL, '/audit');
  const before = new Set(fs.readdirSync(downloadRoot));
  await clickButtonByText(audit, '导出缓存命中 ZIP');
  await delay(120);
  await captureState(audit, dir, 'audit-download-loading.png', ['submitting', 'loading'], covered);
  const after = await waitForNewDownload(before);
  await waitForText(audit, '缓存命中 ZIP 已导出');
  const downloadFile = path.join(dir, 'audit-download-toast.png');
  await audit.screenshot({ path: downloadFile, fullPage: true });
  covered.add('download');
  covered.add('success toast');
  log('download_capture', { states: ['download', 'success toast'], downloads: after, file: path.relative(workspaceRoot, downloadFile), metrics: await pageMetrics(audit) });
  await audit.close();
  return [...covered].sort();
}

async function main() {
  mkdirs();
  const port = await findPort(Number(process.env.UI_REVIEW_PORT || 5192));
  const baseURL = `http://127.0.0.1:${port}/console`;
  const server = startServer(port);
  log('server_start', { port, baseURL });
  try {
    await waitForServer(server);
    const { browser, sessionReused } = await loginAndVerifyReuse(baseURL);
    try {
      for (const theme of themes) {
        for (const viewport of viewports) {
          for (const [name, route] of adminPages) {
            await capturePage(browser, baseURL, 'admin', theme, viewport, name, route);
          }
          for (const [name, route] of userPages) {
            await capturePage(browser, baseURL, 'user', theme, viewport, name, route);
          }
        }
      }
      const coveredStates = await captureStates(browser, baseURL);
      log('coverage', {
        session_reused: sessionReused,
        viewports: viewports.map((v) => v.name),
        themes,
        admin_pages: adminPages.map(([name]) => name),
        user_pages: userPages.map(([name]) => name),
        states: coveredStates,
      });
      if (!sessionReused) {
        throw new Error('Persistent profile did not reuse the fixture session');
      }
    } finally {
      await browser.close();
    }
  } finally {
    await stopServer(server);
    fs.writeFileSync(path.join(outDir, 'operation-log.json'), `${JSON.stringify(operationLog, null, 2)}\n`);
  }
  console.log(`UI review capture written to ${path.relative(workspaceRoot, outDir)}`);
}

main().catch((error) => {
  log('fatal', { message: error.message, stack: error.stack });
  fs.mkdirSync(outDir, { recursive: true });
  fs.writeFileSync(path.join(outDir, 'operation-log.json'), `${JSON.stringify(operationLog, null, 2)}\n`);
  console.error(error);
  process.exit(1);
});
