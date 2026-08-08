import fs from 'node:fs';
import net from 'node:net';
import path from 'node:path';
import process from 'node:process';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import puppeteer from 'puppeteer';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workspaceRoot = path.resolve(root, '..');
const distRoot = path.join(workspaceRoot, 'internal', 'console', 'dist');
const outDir = path.join(workspaceRoot, '.run', 'ui-review');
const screenshotRoot = path.join(outDir, 'screenshots');
const downloadRoot = path.join(outDir, 'downloads');
const userDataDir = path.join(outDir, 'chrome-profile');
const viteBin = path.join(root, 'node_modules', 'vite', 'bin', 'vite.js');
const serverReadyPattern = /Local:\s+http:\/\/127\.0\.0\.1:/;
const seed = 'ui-review-v6.1-fixed-seed';
const recordOnly = process.env.UI_REVIEW_RECORD_ONLY === '1';
const skipStates = process.env.UI_REVIEW_SKIP_STATES === '1';
const staticMode = process.env.UI_REVIEW_STATIC === '1';

const viewports = [
  { name: '1440x900', width: 1440, height: 900 },
  { name: '1280x720', width: 1280, height: 720 },
  { name: '820x1180', width: 820, height: 1180 },
  { name: '390x844', width: 390, height: 844, mobile: true },
  { name: '360x800', width: 360, height: 800, mobile: true },
];
const themes = ['light', 'dark'];
const adminPages = [
  ['Dashboard', '/'],
  ['Accounts', '/accounts'],
  ['Groups', '/groups'],
  ['Providers', '/providers'],
  ['Models', '/models'],
  ['PublicChat', '/public-chat'],
  ['Egress', '/egress'],
  ['UpstreamErrors', '/upstream-error-rules'],
  ['Registration', '/registration'],
  ['TeamLifecycle', '/team-lifecycle'],
  ['EmailPool', '/email-pool'],
  ['CloudflareMailbox', '/email-pool/cloudflare'],
  ['Usage', '/usage'],
  ['Quota', '/quota'],
  ['ModelQuality', '/model-quality'],
  ['System', '/system'],
  ['CFEvents', '/cf-events'],
  ['Audit', '/audit'],
  ['Keys', '/keys'],
  ['Users', '/users'],
  ['Settings', '/settings-v2'],
  ['AIChatGPT', '/settings/ai/chatgpt'],
  ['AIClaude', '/settings/ai/claude'],
  ['AIKiro', '/settings/ai/kiro'],
  ['AIAntigravity', '/settings/ai/antigravity'],
  ['AICodex', '/settings/ai/codex'],
  ['AIClaudeCode', '/settings/ai/claude-code'],
];
const userPages = [
  ['PortalDashboard', '/portal'],
  ['PortalKeys', '/portal/keys'],
  ['PortalModels', '/portal/models'],
  ['PortalProfile', '/portal/profile'],
];

function requested(envName) {
  return String(process.env[envName] || '').split(',').map((item) => item.trim()).filter(Boolean);
}

function selected(value, envName) {
  const values = requested(envName);
  return !values.length || values.includes(value);
}

// A filter naming something that does not exist used to select nothing, capture only the
// state shots, print the success line and exit 0 — so a run that reviewed none of the pages
// asked for was indistinguishable from one that reviewed all of them. These are page names,
// and passing a route (a natural mistake) matches nothing at all.
function assertKnownFilter(envName, known) {
  const unknown = requested(envName).filter((value) => !known.includes(value));
  if (!unknown.length) return;
  throw new Error(`${envName} names nothing that exists: ${unknown.join(', ')}\n  known values: ${known.join(', ')}`);
}

const knownPageNames = [...adminPages, ...userPages].map(([name]) => name);
assertKnownFilter('UI_REVIEW_VIEWPORTS', viewports.map((viewport) => viewport.name));
assertKnownFilter('UI_REVIEW_THEMES', themes);
assertKnownFilter('UI_REVIEW_PAGES', knownPageNames);

const activeViewports = viewports.filter((viewport) => selected(viewport.name, 'UI_REVIEW_VIEWPORTS'));
const activeThemes = themes.filter((theme) => selected(theme, 'UI_REVIEW_THEMES'));
const activeAdminPages = adminPages.filter(([name]) => selected(name, 'UI_REVIEW_PAGES'));
const activeUserPages = userPages.filter(([name]) => selected(name, 'UI_REVIEW_PAGES'));

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
      upstream_account_id: 'team-parent-fixture-001',
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
  // Spread across the families ModelNameList groups by, including one name that matches
  // no prefix, so the "其他" bucket is exercised too.
  modelNames: [
    'gpt-5.5', 'gpt-5.5-mini', 'gpt-5.4', 'gpt-4.1', 'o4-mini', 'codex-mini-latest',
    'claude-opus-4-6-20260501', 'claude-sonnet-4-5-20250929', 'claude-haiku-4-5-20251001',
    'gemini-3.0-pro', 'gemini-2.5-flash',
    'deepseek-v3.2', 'qwen3-max', 'glm-5', 'kimi-k2-turbo',
    'llama-4-maverick', 'mistral-large-2', 'grok-4',
    'text-embedding-3-large', 'internal-eval-harness-v2',
  ],
  configFields: [
    { key: 'require_downstream_key', label: '下游 Key 必填', category: '访问控制', type: 'bool', effect: 'hot', options: [], help: '要求普通请求携带用户 API Key。', value: true, overridden: true },
    { key: 'conversation_isolation', label: '会话隔离', category: '访问控制', type: 'bool', effect: 'hot', options: [], help: '按账号隔离上游会话标识。', value: true, overridden: false },
    { key: 'claude_cache_ttl', label: 'Claude 缓存 TTL', category: '缓存', type: 'select', effect: 'hot', options: ['', '5m', '1h'], help: 'Claude prompt cache control TTL。', value: '1h', overridden: true },
  ],
  // PublicChat had no fixture, so /admin/public-chat/links fell through to the catch-all `{}`
  // at the bottom of handleAPI and the page rendered its empty state in every single capture —
  // the list half of that screen has never once been photographed. Both enabled and disabled
  // are present so each Tag colour is exercised, one name is long enough to test truncation in
  // the summary column, and the two route_type branches are covered because they print through
  // different sides of routeSummary().
  publicChatLinks: [
    {
      id: 'pcl_support',
      slug: 'support-chat',
      name: '官网客服',
      title: '在线客服',
      enabled: true,
      route_type: 'user_group',
      user_group_id: 'ug_fixture',
      route_label: 'Fixture users',
      model: 'gpt-5.5',
      welcome_message: '您好，请描述遇到的问题。',
      max_history_messages: 24,
      rate_limit_per_minute: 30,
      public_url: 'https://pool.example.test/chat/support-chat',
    },
    {
      id: 'pcl_trial',
      slug: 'trial-desk-with-a-long-slug-for-truncation',
      name: '试用咨询入口（长名称用于验证摘要列截断）',
      title: '试用咨询',
      enabled: false,
      route_type: 'account_pool_group',
      group_name: 'staging',
      route_label: 'staging',
      model: 'claude-sonnet-4-5-20250929',
      welcome_message: '',
      max_history_messages: 12,
      rate_limit_per_minute: 10,
      public_url: 'https://pool.example.test/chat/trial-desk-with-a-long-slug-for-truncation',
    },
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
  fs.writeFileSync(path.join(outDir, 'browser-script.mjs'), fs.readFileSync(fileURLToPath(import.meta.url)));
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
  const byProviderModel = [
    { provider: 'codex', provider_id: 'codex', provider_name: 'Codex', model: 'gpt-5.5', display_label: 'Codex · gpt-5.5', requests: 1234, prompt_tokens: 7100000, completion_tokens: 890000, total_tokens: 7990000, cached_tokens: 5900000, cache_input_tokens: 10300000, cache_read_tokens: 5900000, cache_creation_tokens: 320000 },
    { provider: 'claude', provider_id: 'claude', provider_name: 'Claude', model: 'claude-sonnet-4', display_label: 'Claude · Sonnet 4', requests: 98, prompt_tokens: 4200000, completion_tokens: 180000, total_tokens: 4380000, cached_tokens: 1200, cache_input_tokens: 6300000, cache_read_tokens: 1200, cache_creation_tokens: 2100000 },
  ];
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
    by_account: [
      { account_id: fixtures.accounts[0].id, requests: 1234, prompt_tokens: 7100000, completion_tokens: 890000, actual_prompt_tokens: 7100000, actual_completion_tokens: 890000, total_tokens: 7990000, hit_requests: 800, request_hit_rate: 0.65, real_token_hit_rate: 0.52, cache_read_tokens: 5900000, cache_creation_tokens: 320000, cache_miss_tokens: 1200000 },
      { account_id: fixtures.accounts[1].id, requests: 98, prompt_tokens: 4200000, completion_tokens: 180000, actual_prompt_tokens: 4200000, actual_completion_tokens: 180000, total_tokens: 4380000, hit_requests: 12, request_hit_rate: 0.12, real_token_hit_rate: 0.01, cache_read_tokens: 1200, cache_creation_tokens: 2100000, cache_miss_tokens: 4200000 },
      { account_id: fixtures.accounts[2].id, requests: 12, prompt_tokens: 260000, completion_tokens: 31000, actual_prompt_tokens: 260000, actual_completion_tokens: 31000, total_tokens: 291000, hit_requests: 0, request_hit_rate: 0, real_token_hit_rate: 0, cache_read_tokens: 0, cache_creation_tokens: 41000, cache_miss_tokens: 260000 },
    ],
    by_account_model: [
      { account_id: fixtures.accounts[0].id, model: 'gpt-5.5', requests: 1234, hit_requests: 800, request_hit_rate: 0.65, token_hit_rate: 0.52, real_token_hit_rate: 0.52, cache_read_tokens: 5900000, cache_creation_tokens: 320000, cache_miss_tokens: 1200000 },
      { account_id: fixtures.accounts[1].id, model: 'claude-sonnet-4', requests: 98, hit_requests: 12, request_hit_rate: 0.12, token_hit_rate: 0.01, real_token_hit_rate: 0.01, cache_read_tokens: 1200, cache_creation_tokens: 2100000, cache_miss_tokens: 4200000 },
    ],
    by_provider: [
      { provider: 'codex', provider_id: 'codex', provider_name: 'Codex', requests: 1234, prompt_tokens: 7100000, cached_tokens: 5900000, cache_input_tokens: 10300000, cache_read_tokens: 5900000, cache_creation_tokens: 320000 },
      { provider: 'claude', provider_id: 'claude', provider_name: 'Claude', requests: 98, prompt_tokens: 4200000, cached_tokens: 1200, cache_input_tokens: 6300000, cache_read_tokens: 1200, cache_creation_tokens: 2100000 },
    ],
    by_provider_model: byProviderModel,
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
  const buckets = Array.from({ length: 8 }, (_, i) => ({
    bucket: now - (7 - i) * 3600,
    requests: 60 + i * 12,
    prompt_tokens: 120000 + i * 15000,
    completion_tokens: 22000 + i * 4000,
    cached_tokens: 80000 + i * 12000,
    cache_read_tokens: 80000 + i * 12000,
    cache_creation_tokens: 9000 + i * 1000,
    total_tokens: 142000 + i * 19000,
  }));
  return {
    ...usageWindow(req),
    since: dayStart,
    now,
    bucket: 3600,
    buckets,
    model_series: buckets.flatMap((bucket, index) => [
      { ...bucket, series_key: 'codex:gpt-5.5', series_label: 'Codex · gpt-5.5', provider_id: 'codex', provider_name: 'Codex', model: 'gpt-5.5', requests: 48 + index * 9, total_tokens: 112000 + index * 15000 },
      { ...bucket, series_key: 'claude:sonnet-4', series_label: 'Claude · Sonnet 4', provider_id: 'claude', provider_name: 'Claude', model: 'claude-sonnet-4', requests: 12 + index * 3, total_tokens: 30000 + index * 4000 },
    ]),
    series: [
      { series_dimension: 'provider_model', series_key: 'codex:gpt-5.5', series_label: 'Codex · gpt-5.5', provider_id: 'codex', provider_name: 'Codex', model: 'gpt-5.5' },
      { series_dimension: 'provider_model', series_key: 'claude:sonnet-4', series_label: 'Claude · Sonnet 4', provider_id: 'claude', provider_name: 'Claude', model: 'claude-sonnet-4' },
    ],
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
  if (p === '/admin/config' && url.searchParams.get('placement') === 'ai_settings') {
    const domain = url.searchParams.get('domain') || 'chatgpt';
    return req.respond(json([
      { key: `${domain}_model`, label: '默认模型', category: '模型', type: 'string', effect: 'hot', help: '该客户端未显式指定模型时使用。', placement: 'ai_settings', domain, scope: 'model', section: 'model', order: 10, value: `${domain}-default`, overridden: true },
      { key: `${domain}_reasoning_effort`, label: '推理强度', category: '模型', type: 'select', effect: 'hot', options: ['low', 'medium', 'high'], help: '只覆盖支持该参数的请求。', placement: 'ai_settings', domain, scope: 'model', section: 'model', order: 20, value: 'medium' },
      { key: `${domain}_enabled`, label: '启用客户端配置', category: '运行', type: 'bool', effect: 'hot', help: '关闭后回退到全局配置。', placement: 'ai_settings', domain, scope: 'global', section: 'runtime', order: 30, value: true },
      { key: `${domain}_request_timeout`, label: '请求超时（秒）', category: '运行', type: 'int', effect: 'restart', help: '长推理请求的上限。', placement: 'ai_settings', domain, scope: 'global', section: 'runtime', order: 40, value: 120 },
    ]));
  }
  if (p === '/admin/config') return req.respond(json(fixtures.configFields));
  if (p === '/admin/settings') return req.respond(json({ conversation_isolation: true, claude_cache_control_inject: true, require_downstream_key: true }));
  if (p === '/admin/settings-center') return req.respond(json({
    config: { values: { require_downstream_key: true, web_search_enabled: true, claude_cache_ttl: '1h' } },
    registrar: {
      default_register_method: 'protocol_v2', registration_concurrency: 2,
      phoneCountryCode: 'BR', proxyHost: 'proxy.example.test', proxyPort: 3010,
      proxyUsername: '', proxyUsername_configured: true,
      proxyPassword: '', proxyPassword_configured: true,
    },
    automation: { policy: { enabled: true, type: 'refill', config: { target: 8, threshold: 2 } } },
    logging: { values: {} },
    memory: { values: {} },
  }));
  if (p === '/admin/accounts/summary') return req.respond(json({ total: 3, active: 1, quarantined: 1, cooling: 0, recheck: 1, codex: 1, claude: 1, other: 1 }));
  if (p === '/admin/accounts') return req.respond(json({ accounts: fixtures.accounts, rows: fixtures.accounts, total: fixtures.accounts.length }));
  if (p === '/admin/groups') return req.respond(json({ groups: [{ name: 'cyber', force_model: 'gpt-5.5', force_effort: 'high' }, { name: 'staging', force_model: '', force_effort: '' }] }));
  if (p === '/admin/user-groups') return req.respond(json([{ id: 'ug_fixture', name: 'Fixture users', targets: [{ kind: 'account_pool_group', id: 'cyber' }] }]));
  if (p === '/admin/api-keys') return req.respond(json({ keys: fixtures.apiKeys }));
  if (p === '/admin/users') return req.respond(json({ users: fixtures.users }));
  if (p === '/admin/providers') return req.respond(json({ providers: [{ id: 'openai', name: 'OpenAI', base_url: 'https://api.openai.com/v1', enabled: true, auto_discover_models: true, models: ['gpt-5.5', 'gpt-5.4'] }] }));
  // Without these the model pages review as their own empty state, which says nothing
  // about how they render a real capability snapshot.
  if (p === '/admin/models' || p === '/user/models') return req.respond(json({ models: fixtures.modelNames, generated_at: Math.floor(Date.now() / 1000) - 240 }));
  if (p === '/admin/register/providers') return req.respond(json({ providers: [
    { type: 'sms', key: 'smsactivate', display_name: 'SMS-Activate', enabled: true, priority: 20, config: { api_key: '', api_key_configured: true, service: 'dr', max_price: '0.50' } },
    { type: 'mailbox', key: 'imap', display_name: 'IMAP', enabled: true, priority: 10, config: { host: 'imap.example.test', port: '993', email: '', email_configured: true, password: '', password_configured: true, use_tls: true } },
    { type: 'captcha', key: 'yescaptcha', display_name: 'YesCaptcha', enabled: true, priority: 10, config: { api_key: '', api_key_configured: true } },
    { type: 'email', key: 'hotmail_otp', display_name: 'Hotmail OTP', enabled: true, priority: 10, config: { base_email: '', base_email_configured: true, otp_url: '', otp_url_configured: true, auth_token: '', auth_token_configured: true } },
  ] }));
  if (p === '/admin/register/providers/options') return req.respond(json({ sms: ['smsbower', 'herosms'], mailbox: ['cloudflare'], captcha: ['capsolver'] }));
  // health values are the ones scheduler.EgressHealthy actually recognises:
  // "", healthy, cooldown, tripped are schedulable; disabled is not.
  if (p === '/admin/egress-profiles') return req.respond(json({ profiles: [
    // The proxy endpoint field is `endpoint` (storage.EgressProfile), not proxy_url, and
    // exit_ip is what the network column leads with — without it every row read as "—".
    { id: 'egress_direct', name: 'Direct', type: 'direct', region: 'us-east', health: 'healthy', latency_millis: 42, stream_capable: true, max_concurrency: 8, exit_ip: '203.0.113.17' },
    { id: 'egress_resi_us', name: 'Residential US · sticky session pool', type: 'http', region: 'us-west', health: 'healthy', latency_millis: 188, stream_capable: true, max_concurrency: 4, endpoint: 'http://resi.example.test:8080', proxy_auth_mode: 'credential', exit_ip: '198.51.100.204' },
    { id: 'egress_socks_eu', name: 'SOCKS5 EU', type: 'socks5', region: 'eu-central', health: 'tripped', latency_millis: 604, stream_capable: false, max_concurrency: 2, cooldown_until: Math.floor(Date.now() / 1000) + 900, endpoint: 'socks5://eu.example.test:1080', proxy_auth_mode: 'api_whitelist', exit_ip: '192.0.2.88' },
    { id: 'egress_warp', name: 'WARP sidecar', type: 'warp', region: 'auto', health: 'disabled', latency_millis: 0, stream_capable: true, max_concurrency: 0 },
  ] }));
  // Shape follows storage.EgressPool: members are objects carrying egress_id and an
  // embedded egress, and the strategy field is assignment_strategy. The previous fixture
  // used bare id strings and `strategy`, so the members column rendered as a stray comma.
  if (p === '/admin/egress-pools') return req.respond(json({ pools: [
    { id: 'pool_registration', name: 'Registration Pool', purpose: 'registration', assignment_strategy: 'sticky_least_used', members: [
      { pool_id: 'pool_registration', egress_id: 'egress_direct', enabled: true, capacity: 4, egress: { id: 'egress_direct', name: 'Direct' } },
      { pool_id: 'pool_registration', egress_id: 'egress_resi_us', enabled: true, capacity: 1, egress: { id: 'egress_resi_us', name: 'Residential US · sticky session pool' } },
    ] },
    { id: 'pool_runtime', name: 'Runtime Pool', purpose: 'runtime', assignment_strategy: 'round_robin', members: [
      { pool_id: 'pool_runtime', egress_id: 'egress_direct', enabled: true, capacity: 8, egress: { id: 'egress_direct', name: 'Direct' } },
      { pool_id: 'pool_runtime', egress_id: 'egress_socks_eu', enabled: false, capacity: 2, egress: { id: 'egress_socks_eu', name: 'SOCKS5 EU' } },
    ] },
  ] }));
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
  if (p === '/admin/quota') return req.respond(json([
    // Mirrors codex-backend RateLimitStatusPayload: `credits` (extra paid balance) and
    // `spend_control` are siblings of `rate_limit`, and only Codex reports them.
    { account_id: fixtures.accounts[0].id, provider: 'codex', model: 'gpt-5.5', used_percent: 62, remaining_tokens: 123456, limit_tokens: 999999, status: 'ok', quota_summary: { sync_reason: 'ok', primary: { used_percent: 62, remaining_tokens: 123456, reset_at: now + 5400 }, secondary: { used_percent: 41 }, credits: { has_credits: true, unlimited: false, balance: '$12.50', spend_control_reached: false, source: 'workspace', limit: '$100.00', used: '$37.00', remaining: '$63.00', used_percent: 37, remaining_percent: 63, reset_at: now + 172800, status: 'ok', updated_at: now - 300 }, reset_credits: { status: 'ok', available_count: 4, source: 'rate-limit-reset-credits', updated_at: now - 300 } } },
    { account_id: fixtures.accounts[1].id, provider: 'claude', model: 'claude-sonnet-4', used_percent: 94, remaining_tokens: 8200, limit_tokens: 500000, status: 'error/token_expired', quota_summary: { sync_reason: 'error/token_expired', primary: { used_percent: 94, remaining_tokens: 8200, reset_at: now + 900 }, secondary: { used_percent: 88 } } },
    { account_id: fixtures.accounts[2].id, provider: 'codex', model: '', used_percent: 76, remaining_tokens: 64000, limit_tokens: 300000, status: 'ok', quota_summary: { sync_reason: 'ok', primary: { used_percent: 76, remaining_tokens: 64000, reset_at: now + 14400 }, secondary: { used_percent: 55 }, credits: { has_credits: false, unlimited: false, balance: '$0.00', spend_control_reached: true, source: 'workspace', limit: '$50.00', used: '$50.00', remaining: '$0.00', used_percent: 100, remaining_percent: 0, reset_at: now + 86400, status: 'spend_limit_reached', updated_at: now - 120 }, reset_credits: { status: 'ok', available_count: 0, source: 'rate-limit-reset-credits', updated_at: now - 120 } } },
  ]));
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
  if (p === '/admin/model-quality') return req.respond(json({
    enabled: true,
    running: false,
    interval_minutes: 60,
    reasoning_effort: 'medium',
    degraded_threshold: 2,
    statuses: [
      { group_name: 'cyber', model: 'gpt-5.5', provider: 'codex', state: 'healthy', last_outcome: 'match', consecutive_anomalies: 0, last_actual: '42', last_expected: '42', last_returned_model: 'gpt-5.5', total_tokens: 184000, last_latency_ms: 812, last_probe_at: now - 900 },
      { group_name: 'staging', model: 'claude-sonnet-4', provider: 'claude', state: 'suspect', last_outcome: 'mismatch', consecutive_anomalies: 1, last_actual: '41', last_expected: '42', last_returned_model: 'claude-sonnet-4-20250514', total_tokens: 96000, last_latency_ms: 1540, last_probe_at: now - 1800 },
      { group_name: '', model: 'gpt-5-mini-with-a-very-long-model-identifier', provider: 'custom', state: 'degraded', last_outcome: 'mismatch', consecutive_anomalies: 3, last_actual: '', last_expected: '42', last_returned_model: '', total_tokens: 12000, last_latency_ms: 4200, last_probe_at: now - 5400 },
    ],
    runs: [
      { created_at: now - 900, group_name: 'cyber', model: 'gpt-5.5', phase: 'primary', outcome: 'match', actual: '42', expected: '42', returned_model: 'gpt-5.5', total_tokens: 1800, latency_ms: 812 },
      { created_at: now - 1800, group_name: 'staging', model: 'claude-sonnet-4', phase: 'confirmation', outcome: 'mismatch', actual: '41', expected: '42', returned_model: 'claude-sonnet-4-20250514', total_tokens: 2100, latency_ms: 1540, error_kind: '' },
      { created_at: now - 5400, group_name: '', model: 'gpt-5-mini-with-a-very-long-model-identifier', phase: 'primary', outcome: 'error', actual: '', expected: '42', returned_model: '', total_tokens: 0, latency_ms: 4200, error_kind: 'upstream_timeout', error_message: 'fixture upstream timeout after 4200ms' },
    ],
  }));
  if (p === '/admin/cf-events') return req.respond(json([{ id: 1, created_at: now - 60, account_id: fixtures.accounts[0].id, status: 403, cf_ray: 'fixture-ray', category: 'challenge', message: 'fixture challenge' }]));
  if (p === '/admin/register/readiness') return req.respond(json({
    ready: true,
    providers: { mailbox: 1, email_otp: 1, sms: 2, captcha: 1 },
    blockers: [],
    warnings: ['低命中高消耗 fixture'],
    pool: { id: 'pool_registration', name: 'Registration Pool', members: 1 },
  }));
  if (p === '/admin/register/stats' || p === '/admin/automation/stats') return req.respond(json({
    pending: 1,
    running: 0,
    succeeded: 7,
    failed: 1,
    last_error: '',
    totals: { success_rate: 0.875, succeeded: 7, failed: 1 },
    by_day: [
      { date: '2026-07-29', succeeded: 2, failed: 1 },
      { date: '2026-07-30', succeeded: 3, failed: 0 },
      { date: '2026-07-31', succeeded: 2, failed: 0 },
    ],
  }));
  if (p === '/admin/register/countries') return req.respond(json([
    { isoCode: 'US', name: 'United States', nameZh: '美国' },
    { isoCode: 'JP', name: 'Japan', nameZh: '日本' },
  ]));
  // Every status the page counts separately gets a non-zero row: idle/ready roll up into
  // 可用, and in_use / used / error are their own metrics. With only idle and error present
  // three of the five rail tracks reviewed as a flat 0, which says nothing about how the
  // rail reads when the proportions actually differ.
  if (p === '/admin/email-pool') return req.respond(json({
    accounts: [
      { id: 'mail-primary-fixture-001', email: 'registration.primary.with.long.alias@example-very-long-domain.test', client_id: 'cloudflare-primary', status: 'idle', group_name: 'registration', created_at: now - 86400, updated_at: now - 120 },
      { id: 'mail-ready-fixture-006', email: 'standby@example.test', client_id: 'cloudflare-secondary', status: 'ready', group_name: 'registration', created_at: now - 43200, updated_at: now - 300 },
      { id: 'mail-inuse-fixture-003', email: 'claiming.now@example.test', client_id: 'cloudflare-primary', status: 'in_use', group_name: 'registration', last_used_at: now - 45, created_at: now - 7200, updated_at: now - 45 },
      { id: 'mail-used-fixture-004', email: 'spent.alias@example.test', client_id: 'cloudflare-secondary', status: 'used', group_name: 'team', last_used_at: now - 90000, created_at: now - 604800, updated_at: now - 90000 },
      { id: 'mail-used-fixture-005', email: 'spent.second@example.test', client_id: 'cloudflare-secondary', status: 'used', group_name: 'team', last_used_at: now - 250000, created_at: now - 900000, updated_at: now - 250000 },
      { id: 'mail-error-fixture-002', email: 'retry@example.test', client_id: 'cloudflare-primary', status: 'error', group_name: 'team', error_message: 'upstream mailbox token expired; rotate the admin token and retry', last_used_at: now - 3600, created_at: now - 172800, updated_at: now - 60 },
    ],
    total: 6,
    page: 1,
    page_size: 20,
    counts: { idle: 1, ready: 1, in_use: 1, used: 2, error: 1 },
  }, 200, { 'x-request-id': 'ui-review-email-pool-001' }));
  if (p === '/admin/email-pool/cloudflare') return req.respond(json({
    profiles: [{
      provider_key: 'cloudflare-primary', display_name: 'Cloudflare · example.com', api_url: 'https://mail.example.com', domain: 'example.com', enabled: true,
      admin_token_configured: true, default_for_registration: true, default_for_team: true, updated_at: now - 300,
      health: { provider_key: 'cloudflare-primary', last_status: 'healthy', last_checked_at: now - 30, latency_ms: 86, success_count: 42, failure_count: 1, consecutive_failures: 0 },
    }],
    defaults: { registration: 'cloudflare-primary', team: 'cloudflare-primary' },
    deployment: {
      recommended_adapter: 'cloudflare-email-worker-d1', repository_path: 'deploy/cloudflare-mailbox',
      quickstart: ['cd deploy/cloudflare-mailbox', 'npm install', './deploy.sh'],
      steps: ['创建 D1 数据库并执行迁移', '设置 ADMIN_TOKEN 与 TOKEN_SECRET', '部署 Worker', '配置 Email Routing catch-all', '回到控制台保存并测试'],
      references: ['https://developers.cloudflare.com/email-routing/'],
    },
  }));
  if (p === '/admin/team-lifecycle/workspaces') return req.respond(json({
    items: [{ id: 'workspace-fixture-001', name: 'Production Team', provider: 'openai', parent_account_id: fixtures.accounts[0].id, mailbox_domain: 'example.com', desired_seats: 8, active: true }],
  }));
  if (p === '/admin/team-lifecycle/workflows') return req.respond(json({ items: [] }));
  if (p === '/admin/team-lifecycle/stats') return req.respond(json({
    states: { active: 0, queued: 0, retry_wait: 0, review_required: 0, completed: 0 },
    readiness: {
      ready: true, workspace_create_ready: true, cycle_create_ready: true, parent_accounts: 1, mailbox_profiles: 1,
      mailbox_default_configured: true, mailbox_healthy: true, registration_ready: true, registration_method: 'protocol_v2', workspaces: 1, blockers: [],
    },
  }));
  if (p === '/admin/export/cache-hits') {
    await delay(650);
    if (hasFixture(req, 'fixture_export_fail')) return req.respond(json({ error: { message: 'fixture export failure' } }, 500));
    return req.respond({ status: 200, headers: { 'content-type': 'application/zip', 'content-disposition': 'attachment; filename="codex-pool-cache-hits-fixture.zip"' }, body: Buffer.from('PK\u0003\u0004cache-hit-zip-fixture') });
  }
  if (p === '/admin/export/logs') return req.respond({ status: 200, headers: { 'content-type': 'application/zip', 'content-disposition': 'attachment; filename="codex-pool-diagnostics-fixture.zip"' }, body: Buffer.from('PK\u0003\u0004diagnostics-zip-fixture') });
  if (p === '/user/usage') return req.respond(json([{ model: 'gpt-5.5', requests: 18, prompt_tokens: 12000, completion_tokens: 800, total_tokens: 12800, cached_tokens: 4200, cache_input_tokens: 12000, cache_read_tokens: 4200, cache_creation_tokens: 700 }]));
  if (p === '/user/usage/timeseries') return req.respond(json({ buckets: timeseries(req).buckets.slice(0, 5) }));
  if (p === '/user/api-keys') return req.respond(json([{ label: 'my-portal-key-with-long-label', key_hash: 'portal1234567890abcdef', enabled: true, created_at: now - 1200, secret: 'cap_user_fixture' }]));
  if (p === '/user/profile') return req.respond(json({ email: 'user@example.test', name: 'User Fixture With Long Display Name', role: 'user' }));
  if (p === '/admin/public-chat/links') return req.respond(json({ links: fixtures.publicChatLinks }));
  if (p.startsWith('/admin/') || p.startsWith('/user/')) return req.respond(json({}));
  return req.continue();
}

export async function installMocks(page) {
  await page.setRequestInterception(true);
  page.on('request', async (req) => {
    try {
      if (staticMode) {
        const url = new URL(req.url());
        if (url.pathname.startsWith('/console/assets/')) {
          const relative = url.pathname.slice('/console/'.length);
          const file = path.resolve(distRoot, relative);
          const withinDist = path.relative(distRoot, file);
          if (withinDist.startsWith('..') || path.isAbsolute(withinDist) || !fs.existsSync(file)) return req.respond({ status: 404, body: '' });
          const contentType = file.endsWith('.js') ? 'text/javascript; charset=utf-8'
            : file.endsWith('.css') ? 'text/css; charset=utf-8'
              : 'application/octet-stream';
          return req.respond({ status: 200, contentType, body: fs.readFileSync(file) });
        }
        if (url.pathname === '/console' || url.pathname === '/console/' || url.pathname.startsWith('/console/')) {
          return req.respond({ status: 200, contentType: 'text/html; charset=utf-8', body: fs.readFileSync(path.join(distRoot, 'index.html')) });
        }
      }
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
  }, theme);
  page.on('response', (response) => {
    if (response.status() >= 400 && !response.url().includes('/auth/me')) {
      const req = response.request();
      const expected = hasFixture(req, 'force403') || hasFixture(req, 'fixture_error') || hasFixture(req, 'fixture_export_fail') || hasFixture(req, 'fixture_login_delay');
      log(expected ? 'expected_http_error' : 'http_error', { status: response.status(), url: response.url() });
    }
  });
  page.on('pageerror', (error) => log('page_error', { role, theme, viewport: viewport.name, message: error.message, stack: error.stack }));
  page.on('console', (message) => {
    if (message.type() === 'error') log('console_error', { role, theme, viewport: viewport.name, message: message.text() });
  });
  return page;
}

async function gotoApp(page, baseURL, route) {
  await page.goto(`${baseURL}${route}`, { waitUntil: 'domcontentloaded', timeout: 45000 });
  await page.waitForFunction(() => {
    const content = document.querySelector('.pool-route-content[data-page-ready="true"]');
    if (!content || content.innerText.trim().length <= 10) return false;
    const title = content.querySelector('h1, .pool-page-title');
    if (!title) return false;
    const rect = title.getBoundingClientRect();
    return rect.width > 1 && rect.height > 1;
  }, { timeout: 45000 });
  await page.waitForNetworkIdle({ idleTime: 250, timeout: 5000 }).catch(() => {});
  await page.waitForFunction(() => {
    const portalShell = document.querySelector('.pool-portal-shell');
    const sider = document.querySelector('.pool-shell-sider')?.getBoundingClientRect();
    const main = document.querySelector('.pool-main-layout')?.getBoundingClientRect();
    if (!main) return false;
    if (portalShell) return !sider && Math.abs(main.x) <= 1 && Math.abs(main.width - innerWidth) <= 1;
    if (!sider) return false;
    if (innerWidth < 768) return Math.abs(main.x) <= 1 && Math.abs(main.width - innerWidth) <= 1;
    const expected = innerWidth < 1360 ? 68 : 248;
    return Math.abs(sider.width - expected) <= 1 && Math.abs(main.x - expected) <= 1 && Math.abs(main.width - (innerWidth - expected)) <= 1;
  }, { timeout: 10000 });
  await page.bringToFront();
  await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  await new Promise((resolve) => setTimeout(resolve, 200));
}

async function screenshotDocument(page, file, { segments = true } = {}) {
  const dimensions = await page.evaluate(() => ({
    height: Math.ceil(document.documentElement.scrollHeight),
    viewportHeight: innerHeight,
  }));
  const captureAt = async (target, y) => {
    await page.evaluate((top) => window.scrollTo(0, top), y);
    await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    await page.screenshot({ path: target });
  };
  const files = [file];
  await captureAt(file, 0);
  if (segments && dimensions.height > dimensions.viewportHeight + 1) {
    const extension = path.extname(file);
    const stem = file.slice(0, -extension.length);
    const maxScroll = dimensions.height - dimensions.viewportHeight;
    const positions = [...new Set([Math.round(maxScroll / 2), maxScroll])].filter((position) => position > 1);
    for (const [index, position] of positions.entries()) {
      const target = `${stem}--${index === positions.length - 1 ? 'bottom' : 'middle'}${extension}`;
      await captureAt(target, position);
      files.push(target);
    }
    await page.evaluate(() => window.scrollTo(0, 0));
  }
  return files;
}

async function pageMetrics(page) {
  return page.evaluate(() => {
    const layoutBox = (selector) => {
      const element = document.querySelector(selector);
      if (!element) return null;
      const rect = element.getBoundingClientRect();
      const style = getComputedStyle(element);
      return {
        selector,
        x: Math.round(rect.x), y: Math.round(rect.y), width: Math.round(rect.width), height: Math.round(rect.height),
        display: style.display, visibility: style.visibility, opacity: style.opacity, transform: style.transform,
      };
    };
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
        let ancestor = node.parentElement;
        let safelyClipped = false;
        while (ancestor && ancestor !== cell) {
          const style = getComputedStyle(ancestor);
          const rect = ancestor.getBoundingClientRect();
          const clipsInline = ['hidden', 'clip'].includes(style.overflowX) || ['hidden', 'clip'].includes(style.overflow);
          if (clipsInline && rect.left >= cellRect.left - 1 && rect.right <= cellRect.right + 1) {
            safelyClipped = true;
            break;
          }
          ancestor = ancestor.parentElement;
        }
        if (!safelyClipped && rects.some((rect) => rect.right > cellRect.right + 1 || rect.left < cellRect.left - 1)) {
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
      // Every drawn primitive in MicroCharts, not just the SVG-based ones. Sparkline and
      // RadialGauge draw SVG; RankedBars, StackedMeter and HeatStrip draw filled divs, and
      // leaving those out reported cards as blank whose bars were plainly on screen —
      // a false alarm that hides the real ones. Keep this in step with MicroCharts.jsx.
      const visibleVisuals = [...card.querySelectorAll([
        'svg path', 'svg rect', 'svg circle',
        '.pool-meter span',
        '.pool-cache-breakdown__bar span',
        '.pool-ranked__track > span',
        '.pool-stacked-meter__track > span',
        '.pool-heatstrip__cell',
      ].join(', '))]
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
    const siblingOverlaps = [];
    const auditedContainers = [...document.querySelectorAll('body *')].filter((element) => {
      if (element.closest('svg, [role="img"], .recharts-wrapper')) return false;
      const style = getComputedStyle(element);
      return ['flex', 'inline-flex', 'grid', 'inline-grid'].includes(style.display) && element.children.length > 1;
    });
    for (const container of auditedContainers) {
      const children = [...container.children].filter((child) => {
        const style = getComputedStyle(child);
        const rect = child.getBoundingClientRect();
        return style.display !== 'none' && style.visibility !== 'hidden' &&
          !['absolute', 'fixed'].includes(style.position) && rect.width > 1 && rect.height > 1;
      });
      for (let leftIndex = 0; leftIndex < children.length; leftIndex += 1) {
        const left = children[leftIndex].getBoundingClientRect();
        for (let rightIndex = leftIndex + 1; rightIndex < children.length; rightIndex += 1) {
          const right = children[rightIndex].getBoundingClientRect();
          const width = Math.min(left.right, right.right) - Math.max(left.left, right.left);
          const height = Math.min(left.bottom, right.bottom) - Math.max(left.top, right.top);
          if (width <= 1 || height <= 1) continue;
          siblingOverlaps.push({
            container: container.className || container.tagName,
            left: children[leftIndex].className || children[leftIndex].tagName,
            right: children[rightIndex].className || children[rightIndex].tagName,
            width: Math.round(width), height: Math.round(height),
          });
        }
      }
    }
    const chartTextOverlaps = [];
    document.querySelectorAll('.pool-chart-card svg').forEach((svg) => {
      const labels = [...svg.querySelectorAll('text')].filter((element) => {
        const style = getComputedStyle(element);
        const rect = element.getBoundingClientRect();
        return (element.textContent || '').trim() && style.display !== 'none' && style.visibility !== 'hidden' && rect.width > 1 && rect.height > 1;
      });
      for (let leftIndex = 0; leftIndex < labels.length; leftIndex += 1) {
        const left = labels[leftIndex].getBoundingClientRect();
        for (let rightIndex = leftIndex + 1; rightIndex < labels.length; rightIndex += 1) {
          const right = labels[rightIndex].getBoundingClientRect();
          const width = Math.min(left.right, right.right) - Math.max(left.left, right.left);
          const height = Math.min(left.bottom, right.bottom) - Math.max(left.top, right.top);
          if (width <= 1 || height <= 1) continue;
          chartTextOverlaps.push({
            left: (labels[leftIndex].textContent || '').trim().slice(0, 40),
            right: (labels[rightIndex].textContent || '').trim().slice(0, 40),
            width: Math.round(width),
            height: Math.round(height),
          });
        }
      }
    });
    const clippedControls = [...document.querySelectorAll('button, [role="button"], .pool-tag, .pool-field__label, h1, h2, h3')]
      .filter((element) => {
        const rect = element.getBoundingClientRect();
        if (rect.width <= 1 || rect.height <= 1 || element.closest('[aria-hidden="true"]')) return false;
        return element.scrollWidth > element.clientWidth + 1 || element.scrollHeight > element.clientHeight + 1;
      })
      .map((element) => ({
        element: element.className || element.tagName,
        text: (element.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 80),
        client: `${element.clientWidth}x${element.clientHeight}`,
        scroll: `${element.scrollWidth}x${element.scrollHeight}`,
      }));
    // A card left as an unreadable sliver at the edge of a horizontal scroll pane.
    //
    // This is the blind spot that let a real defect ship. The dashboard chart row was a
    // `grid-auto-flow: column` carousel with min(86vw, 360px) slides, so on a 390px phone
    // the third card sat 17px inside a 366px pane: a strip of legend dots and half-drawn
    // glyphs pinned to the right edge, reading as damage rather than as content. Every
    // existing check passed it. The overflow lives inside the pane, so documentElement
    // .scrollWidth never grows and noPageOverflow stays true; siblingOverlaps looks for
    // intersecting boxes and these do not intersect; clippedControls asks whether an element
    // clips its OWN text, not whether an ancestor clips it. Only a screenshot showed it.
    //
    // Measured at the slide, not at text nodes. Text granularity turned out to be a pixel
    // lottery: that card's 16px padding put its first glyph at x=377 against a 378px edge,
    // so a straddle test needing 2px inside missed the whole defect by one pixel while the
    // card itself was unambiguously sliced.
    //
    // The threshold is what separates a defect from a deliberate carousel. A slide showing
    // half of itself is an affordance — it says "keep scrolling" and stays legible. A slide
    // showing a fraction of one word is neither, so a sliver is flagged when the visible
    // part is under 25% of the slide AND under 72px, which is narrower than any label on
    // these surfaces. Slides that snap to the full pane width are exempt: paging one at a
    // time never leaves anything half-drawn at rest. Table cells are exempt too — a wide
    // table is one continuous surface that pages horizontally by design, and cutting a
    // column mid-cell is how every data grid behaves.
    const scrollClippedText = [];
    const scrollPaneInventory = [];
    const EDGE_TOLERANCE = 2;
    for (const pane of document.querySelectorAll('*')) {
      if (pane.scrollWidth <= pane.clientWidth + EDGE_TOLERANCE) continue;
      const paneStyle = getComputedStyle(pane);
      if (!/(auto|scroll)/.test(paneStyle.overflowX)) continue;
      if (pane.closest('[aria-hidden="true"]')) continue;
      const paneRect = pane.getBoundingClientRect();
      if (paneRect.width <= 1 || paneRect.height <= 1) continue;
      const rightEdge = paneRect.right - parseFloat(paneStyle.borderRightWidth || 0);
      scrollPaneInventory.push({
        pane: String(pane.className || pane.tagName).slice(0, 60),
        client: pane.clientWidth,
        scroll: pane.scrollWidth,
        overflowX: paneStyle.overflowX,
        snapType: paneStyle.scrollSnapType,
        rightEdge: Math.round(rightEdge),
      });

      const SLIVER_FRACTION = 0.25;
      const SLIVER_PX = 72;
      for (const slide of pane.children) {
        if (/^(TD|TH|TR|THEAD|TBODY|COLGROUP|COL)$/.test(slide.tagName)) continue;
        if (slide.closest('[aria-hidden="true"]')) continue;
        const ss = getComputedStyle(slide);
        if (ss.visibility === 'hidden' || ss.display === 'none' || Number(ss.opacity) === 0) continue;

        const slideRect = slide.getBoundingClientRect();
        if (slideRect.width < 1 || slideRect.height < 1) continue;

        const straddles = slideRect.left < rightEdge - EDGE_TOLERANCE
          && slideRect.right > rightEdge + EDGE_TOLERANCE;
        if (!straddles) continue;

        // Paged one-slide-at-a-time: nothing is half-drawn once the scroll settles.
        if (ss.scrollSnapAlign !== 'none' && Math.abs(slideRect.width - paneRect.width) <= 4) continue;

        const visiblePx = rightEdge - slideRect.left;
        const fraction = visiblePx / slideRect.width;
        if (fraction >= SLIVER_FRACTION || visiblePx >= SLIVER_PX) continue;

        scrollClippedText.push({
          pane: String(pane.className || pane.tagName).slice(0, 60),
          slide: String(slide.className || slide.tagName).slice(0, 60),
          text: (slide.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 60),
          visiblePx: Math.round(visiblePx),
          slideWidth: Math.round(slideRect.width),
          visiblePct: Math.round(fraction * 100),
        });
        if (scrollClippedText.length >= 12) break;
      }
      if (scrollClippedText.length >= 12) break;
    }

    const navigation = performance.getEntriesByType('navigation')[0];
    return {
      textOverflows: textOverflows.slice(0, 12),
      blankChartCards: blankChartCards.slice(0, 8),
      siblingOverlaps: siblingOverlaps.slice(0, 12),
      chartTextOverlaps: chartTextOverlaps.slice(0, 12),
      clippedControls: clippedControls.slice(0, 12),
      scrollClippedText: scrollClippedText.slice(0, 12),
      // Informational: every horizontal scroll pane and how far its content runs past the
      // edge. Not an assertion — a horizontal scroller is legitimate (wide tables page this
      // way). It is recorded so the reviewer can see which panes exist and confirm each one
      // is deliberate, and so scrollClippedText above has context when it fires.
      scrollPanes: scrollPaneInventory.slice(0, 12),
      // The app's main scroll container, measured on its own.
      //
      // noPageOverflow compares documentElement.scrollWidth against the viewport, which cannot
      // see this: .pool-content has `overflow: auto`, so anything too wide is absorbed into ITS
      // scroll range and the document never grows. Usage overflowed 29px here — the whole admin
      // page could be dragged sideways on a phone — and every check passed for the full matrix.
      //
      // Cause was a `display: grid` with no grid-template-columns. The implicit `auto` track is
      // sized to max-content and is never shrunk to fit, so nowrap table values pushed the track
      // past the viewport. Reported with the widest offending descendants because the overflow is
      // always inherited by a chain of stretched children, and the top of that chain is the fix
      // site, not the elements that merely report a wide box.
      shellOverflow: (() => {
        const host = document.querySelector('.pool-content');
        if (!host) return null;
        const overflow = host.scrollWidth - host.clientWidth;
        if (overflow <= 2) return null;
        const blame = [];
        for (const el of host.querySelectorAll('*')) {
          if (el.getBoundingClientRect().width <= host.clientWidth + 2) continue;
          const cs = getComputedStyle(el);
          if (/(auto|scroll)/.test(cs.overflowX)) continue;
          let depth = 0;
          for (let p = el; p && p !== host; p = p.parentElement) depth += 1;
          blame.push({ cls: String(el.className || el.tagName).slice(0, 60), w: Math.round(el.getBoundingClientRect().width), depth });
        }
        blame.sort((a, b) => a.depth - b.depth || b.w - a.w);
        return { overflow, client: host.clientWidth, scroll: host.scrollWidth, blame: blame.slice(0, 6) };
      })(),
      path: location.pathname + location.search,
      textLength: document.body.innerText.length,
      documentWidth: document.documentElement.scrollWidth,
      viewportWidth: window.innerWidth,
      noPageOverflow: document.documentElement.scrollWidth <= window.innerWidth + 1,
      focusedVisible: !!document.querySelector(':focus-visible'),
      domNodes: document.getElementsByTagName('*').length,
      resourceCount: performance.getEntriesByType('resource').length,
      domContentLoadedMS: navigation ? Math.round(navigation.domContentLoadedEventEnd) : 0,
      loadMS: navigation ? Math.round(navigation.loadEventEnd) : 0,
      layout: [
        layoutBox('.pool-shell-sider'), layoutBox('.pool-main-layout'), layoutBox('.pool-shell'),
        layoutBox('.pool-route-content'), layoutBox('.pool-route-content h1, .pool-route-content .pool-page-title'),
      ].filter(Boolean),
    };
  });
}

function assertNoTextOverlap(metrics, label) {
  if (!metrics.noPageOverflow) {
    throw new Error(`${label} has page overflow: ${metrics.documentWidth}px > ${metrics.viewportWidth}px`);
  }
  if (metrics.textOverflows?.length) {
    throw new Error(`${label} has table text overflow: ${JSON.stringify(metrics.textOverflows.slice(0, 4))}`);
  }
  if (metrics.blankChartCards?.length) {
    throw new Error(`${label} has blank chart card: ${JSON.stringify(metrics.blankChartCards.slice(0, 3))}`);
  }
  if (metrics.siblingOverlaps?.length) {
    throw new Error(`${label} has sibling overlap: ${JSON.stringify(metrics.siblingOverlaps.slice(0, 4))}`);
  }
  if (metrics.chartTextOverlaps?.length) {
    throw new Error(`${label} has chart text overlap: ${JSON.stringify(metrics.chartTextOverlaps.slice(0, 4))}`);
  }
  if (metrics.clippedControls?.length) {
    throw new Error(`${label} has clipped control text: ${JSON.stringify(metrics.clippedControls.slice(0, 4))}`);
  }
  if (metrics.scrollClippedText?.length) {
    throw new Error(`${label} has text sliced by a scroll container edge: ${JSON.stringify(metrics.scrollClippedText.slice(0, 4))}`);
  }
  // Unconditional: a horizontal scrollbar on the page container is never intentional. Individual
  // panes inside it may scroll sideways by design (wide tables), which is why scrollPanes stays
  // informational, but the shell that holds the whole page must fit the viewport it was given.
  if (metrics.shellOverflow) {
    throw new Error(`${label} page shell scrolls horizontally by ${metrics.shellOverflow.overflow}px (${metrics.shellOverflow.client}->${metrics.shellOverflow.scroll}); widest culprits: ${JSON.stringify(metrics.shellOverflow.blame)}`);
  }
}

async function assertPageContent(page, name, label) {
  const requirements = {
    Dashboard: ['总览', '可调度账号', '24h Token'],
    Accounts: ['账号池'],
    Groups: ['分组'],
    Providers: ['模型提供商'],
    Models: ['模型列表'],
    PublicChat: ['在线聊天'],
    Egress: ['出口 / 代理'],
    UpstreamErrors: ['上游错误规则'],
    Usage: ['用量分析', 'Top 账号', 'Provider + Model 用量与缓存诊断'],
    Settings: ['设置中心', '通用配置', '下游 Key 必填'],
    Registration: ['自动注册', '启动注册任务', '暂无注册任务'],
    TeamLifecycle: ['团队生命周期', '四项配置'],
    EmailPool: ['邮箱池'],
    CloudflareMailbox: ['Cloudflare 自建邮箱'],
    Quota: ['配额 / 限额'],
    ModelQuality: ['模型智商 / 降智检测'],
    System: ['系统监控', 'CPU 使用率', '资源与模块状态'],
    CFEvents: ['Cloudflare 事件'],
    Audit: ['审计日志'],
    Keys: ['API Keys'],
    Users: ['用户管理'],
    AIChatGPT: ['AI 设置'],
    AIClaude: ['AI 设置'],
    AIKiro: ['AI 设置'],
    AIAntigravity: ['AI 设置'],
    AICodex: ['AI 设置'],
    AIClaudeCode: ['AI 设置'],
    PortalDashboard: ['我的用量', '总 Token', '按模型用量'],
    PortalKeys: ['我的 API Key', '复制 Key'],
    PortalModels: ['可用模型'],
    PortalProfile: ['我的资料', 'user@example.test'],
  };
  const required = requirements[name];
  if (!required) return;
  try {
    await page.waitForFunction((items) => items.every((item) => document.body.innerText.includes(item)), { timeout: 15000 }, required);
  } catch {
    const text = await page.evaluate(() => document.body.innerText.replace(/\s+/g, ' ').trim());
    const missing = required.filter((item) => !text.includes(item));
    throw new Error(`${label} is missing expected content after render: ${missing.join(', ')}; body=${text.slice(0, 500)}`);
  }
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
  const label = `${role}/${theme}/${viewport.name}/${name}`;
  try {
    await gotoApp(page, baseURL, route);
    const files = await screenshotDocument(page, file);
    const issues = [];
    try {
      await assertPageContent(page, name, label);
    } catch (error) {
      issues.push(error.message);
    }
    const metrics = await pageMetrics(page);
    try {
      assertNoTextOverlap(metrics, label);
    } catch (error) {
      issues.push(error.message);
    }
    log('screenshot', { role, theme, viewport: viewport.name, page: name, route, file: path.relative(workspaceRoot, file), files: files.map((item) => path.relative(workspaceRoot, item)), metrics });
    if (issues.length) throw new Error(issues.join(' | '));
  } finally {
    await page.close();
  }
}

async function launchReviewBrowser() {
  return puppeteer.launch({
    headless: 'new',
    userDataDir,
    args: [
      '--no-sandbox', '--disable-dev-shm-usage', '--disable-background-timer-throttling',
      '--disable-backgrounding-occluded-windows', '--disable-renderer-backgrounding',
      '--disable-breakpad', '--disable-crashpad', '--disable-crash-reporter', '--no-crash-upload',
    ],
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

// Actions that live in a dropdown are not buttons: Radix renders each item as a
// role="menuitem" div, and it only exists in the DOM once the trigger is open.
async function clickMenuItemByText(page, triggerText, itemText) {
  await clickButtonByText(page, triggerText);
  const item = await page.waitForFunction((label) => {
    const match = [...document.querySelectorAll('[role="menuitem"]')].find((el) => (el.textContent || '').trim() === label);
    if (!match) return null;
    const rect = match.getBoundingClientRect();
    return rect.width > 0 && rect.height > 0 ? match : null;
  }, { timeout: 45000 }, itemText);
  const element = item.asElement();
  if (!element) throw new Error(`menu item not found: ${itemText}`);
  await element.click();
  return element;
}

async function waitForText(page, text, timeout = 45000) {
  await page.waitForFunction((needle) => document.body.innerText.includes(needle), { timeout }, text);
}

async function captureState(page, dir, filename, states, covered, extra = {}) {
  const file = path.join(dir, filename);
  const { fullDocument = false, ...details } = extra;
  await screenshotDocument(page, file, { segments: fullDocument });
  const metrics = await pageMetrics(page);
  let issue = '';
  try {
    assertNoTextOverlap(metrics, filename);
  } catch (error) {
    issue = error.message;
  }
  states.forEach((state) => covered.add(state));
  log('state_capture', { states, file: path.relative(workspaceRoot, file), metrics, issue, ...details });
  if (issue && !recordOnly) throw new Error(issue);
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

  for (const [name, viewport, theme] of [
    ['settings-registrar-desktop.png', { name: '1440x900', width: 1440, height: 900 }, 'light'],
    ['settings-registrar-mobile.png', { name: '390x844', width: 390, height: 844, mobile: true }, 'dark'],
  ]) {
    const settings = await preparePage(browser, baseURL, 'admin', viewport, theme);
    await gotoApp(settings, baseURL, '/settings-v2');
    await clickButtonByText(settings, '注册器凭据');
    await waitForText(settings, '接码平台');
    await captureState(settings, dir, name, ['settings-registrar'], covered, { fullDocument: true, theme, viewport: viewport.name });
    for (const section of [
      { title: '接码平台', content: 'SMS-Activate', slug: 'sms' },
      { title: '验证码求解器', content: 'YesCaptcha', slug: 'captcha' },
      { title: 'Hotmail OTP', content: '基础邮箱', slug: 'hotmail' },
      { title: '邮箱提供商', content: 'Cloudflare / MoeMail', slug: 'mailbox' },
    ]) {
      await clickButtonByText(settings, section.title);
      await waitForText(settings, section.content);
      await captureState(
        settings,
        dir,
        name.replace('.png', `-${section.slug}.png`),
        [`settings-registrar-${section.slug}`],
        covered,
        { fullDocument: true, theme, viewport: viewport.name },
      );
      await clickButtonByText(settings, section.title);
      await settings.waitForFunction((text) => !document.body.innerText.includes(text), { timeout: 15000 }, section.content);
    }
    await settings.close();
  }

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
  await clickMenuItemByText(failedExport, '导出', '缓存命中 ZIP');
  const exportErrorToast = await failedExport.waitForSelector('.pool-toast--error[role="alert"]', { visible: true, timeout: 15000 });
  const exportErrorText = String(await exportErrorToast.evaluate((element) => element.textContent || '')).trim();
  if (!exportErrorText) throw new Error('export failure toast has no accessible error content');
  await captureState(failedExport, dir, 'audit-export-failed.png', ['export failure', 'error'], covered, { toast_text: exportErrorText });
  await failedExport.close();

  const audit = await preparePage(browser, baseURL, 'admin', { name: '1280x720', width: 1280, height: 720 }, 'dark');
  await gotoApp(audit, baseURL, '/audit');
  const before = new Set(fs.readdirSync(downloadRoot));
  await clickMenuItemByText(audit, '导出', '缓存命中 ZIP');
  await delay(120);
  await captureState(audit, dir, 'audit-download-loading.png', ['submitting', 'loading'], covered);
  const after = await waitForNewDownload(before);
  await waitForText(audit, '缓存命中 ZIP 已导出');
  const downloadFile = path.join(dir, 'audit-download-toast.png');
  await screenshotDocument(audit, downloadFile, { segments: false });
  covered.add('download');
  covered.add('success toast');
  log('download_capture', { states: ['download', 'success toast'], downloads: after, file: path.relative(workspaceRoot, downloadFile), metrics: await pageMetrics(audit) });
  await audit.close();
  return [...covered].sort();
}

async function main() {
  mkdirs();
  const port = staticMode ? null : await findPort(Number(process.env.UI_REVIEW_PORT || 5192));
  const baseURL = staticMode ? 'http://ui-review.local/console' : `http://127.0.0.1:${port}/console`;
  const server = staticMode ? null : startServer(port);
  log('server_start', { port, baseURL, static_mode: staticMode });
  try {
    if (server) await waitForServer(server);
    const { browser, sessionReused } = await loginAndVerifyReuse(baseURL);
    try {
      const failures = [];
      for (const theme of activeThemes) {
        for (const viewport of activeViewports) {
          for (const [name, route] of activeAdminPages) {
            try {
              await capturePage(browser, baseURL, 'admin', theme, viewport, name, route);
            } catch (error) {
              failures.push(error.message);
              log('page_failure', { role: 'admin', theme, viewport: viewport.name, page: name, route, message: error.message });
            }
          }
          for (const [name, route] of activeUserPages) {
            try {
              await capturePage(browser, baseURL, 'user', theme, viewport, name, route);
            } catch (error) {
              failures.push(error.message);
              log('page_failure', { role: 'user', theme, viewport: viewport.name, page: name, route, message: error.message });
            }
          }
        }
      }
      const coveredStates = skipStates ? [] : await captureStates(browser, baseURL);
      log('coverage', {
        session_reused: sessionReused,
        viewports: activeViewports.map((v) => v.name),
        themes: activeThemes,
        admin_pages: activeAdminPages.map(([name]) => name),
        user_pages: activeUserPages.map(([name]) => name),
        states: coveredStates,
        record_only: recordOnly,
      });
      if (!sessionReused) {
        throw new Error('Persistent profile did not reuse the fixture session');
      }
      if (failures.length && !recordOnly) throw new Error(`UI review found ${failures.length} page failures; inspect operation-log.json`);
    } finally {
      await browser.close();
    }
  } finally {
    if (server) await stopServer(server);
    fs.writeFileSync(path.join(outDir, 'operation-log.json'), `${JSON.stringify(operationLog, null, 2)}\n`);
  }
  console.log(`UI review capture written to ${path.relative(workspaceRoot, outDir)}`);
}

// Only run the capture when invoked directly; other tooling imports installMocks.
if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    log('fatal', { message: error.message, stack: error.stack });
    fs.mkdirSync(outDir, { recursive: true });
    fs.writeFileSync(path.join(outDir, 'operation-log.json'), `${JSON.stringify(operationLog, null, 2)}\n`);
    console.error(error);
    process.exit(1);
  });
}
