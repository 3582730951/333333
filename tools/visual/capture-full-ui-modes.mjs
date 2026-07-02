import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import puppeteer from 'puppeteer';

const webRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)));
const workspaceRoot = path.resolve(webRoot, '..');
const timestamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
const outputDir = path.join(workspaceRoot, '.run', 'screenshots', `full-ui-${timestamp}`);
const port = Number(process.env.FULL_UI_PORT || 5193);
const baseURL = `http://127.0.0.1:${port}/console`;
const viteBin = path.join(webRoot, 'node_modules', 'vite', 'bin', 'vite.js');
const serverReadyPattern = /Local:\s+http:\/\/127\.0\.0\.1:/;
const wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const now = Math.floor(Date.now() / 1000);

const adminRoutes = [
  ['01-dashboard', '/'],
  ['02-accounts', '/accounts'],
  ['03-groups', '/groups'],
  ['04-egress', '/egress'],
  ['05-providers', '/providers'],
  ['06-registration', '/registration'],
  ['07-automation', '/settings-v2#automation'],
  ['08-lifecycle', '/lifecycle'],
  ['09-gopay', '/gopay'],
  ['10-usage', '/usage'],
  ['11-quota', '/quota'],
  ['12-system', '/system'],
  ['13-cf-events', '/cf-events'],
  ['14-audit', '/audit'],
  ['15-keys', '/keys'],
  ['16-users', '/users'],
  ['17-thinking', '/thinking'],
  ['18-moderation', '/moderation'],
  ['19-settings', '/settings-v2'],
].map(([name, route]) => ({ name, route, role: 'admin' }));

const portalRoutes = [
  ['20-portal-dashboard', '/portal'],
  ['21-portal-keys', '/portal/keys'],
  ['22-portal-profile', '/portal/profile'],
].map(([name, route]) => ({ name, route, role: 'user' }));

const accounts = [
  { id: 'acct_prod_001', label: 'primary-prod', email: 'primary@example.com', provider: 'codex', group_name: 'cyber', plan_type: 'pro', status: 'active' },
  { id: 'acct_stage_002', label: 'staging-worker', email: 'stage@example.com', provider: 'custom', group_name: 'staging', plan_type: 'team', status: 'disabled', quarantine_until: now + 3600 },
  { id: 'acct_claude_003', label: 'claude-backup', email: 'claude@example.com', provider: 'claude', group_name: 'fallback', plan_type: 'team', status: 'active' },
];
const groups = [
  { name: 'cyber', force_model: 'gpt-5-codex', force_effort: 'medium', virtual_2m_enabled: true },
  { name: 'staging', force_model: '', force_effort: '', virtual_2m_enabled: false },
  { name: 'fallback', force_model: 'claude-sonnet-4', force_effort: 'high', virtual_2m_enabled: false },
];
const egresses = [
  { id: 'direct', name: 'Direct', type: 'direct', region: 'US', exit_ip: '203.0.113.10', health: 'healthy', max_concurrency: 24 },
  { id: 'br-residential', name: 'Brazil Residential', type: 'http_proxy', region: 'BR', exit_ip: '198.51.100.20', proxy_auth_mode: 'credential', health: 'healthy', max_concurrency: 12 },
];
const providers = [
  { id: 'openai', name: 'OpenAI', base_url: 'https://api.openai.com/v1', enabled: true, auto_discover_models: true, models: ['gpt-5-codex', 'gpt-5-mini'] },
  { id: 'edge-lab', name: 'Edge Lab', base_url: 'https://edge.example.com/v1', enabled: false, auto_discover_models: false, models: ['custom-chat'] },
];
const users = [
  { id: 'user_alice', email: 'alice@example.com', name: 'Alice Admin', role: 'admin', status: 'active', created_at: now - 86400 },
  { id: 'user_bob', email: 'bob@example.com', name: 'Bob User', role: 'user', status: 'disabled', created_at: now - 172800 },
];
const apiKeys = [
  { label: 'production-cli', group_name: 'cyber', force_model: 'gpt-5-codex', force_effort: 'medium', secret: '', enabled: true, key_hash: 'hash_prod_abcdef1234567890', created_at: now - 3600 },
  { label: 'qa-smoke', group_name: 'staging', force_model: '', force_effort: '', secret: '', enabled: false, key_hash: 'hash_qa_1234567890abcdef', created_at: now - 7200 },
];
const buckets = Array.from({ length: 12 }, (_, i) => {
  const prompt = 42000 + i * 3500;
  const completion = 18000 + i * 1800;
  const cached = 8000 + i * 1000;
  return {
    bucket: now - (11 - i) * 3600,
    prompt_tokens: prompt,
    completion_tokens: completion,
    cached_tokens: cached,
    total_tokens: prompt + completion + cached,
    requests: 80 + i * 9,
  };
});
const usageRows = accounts.map((account, i) => ({
  account_id: account.id,
  label: account.label,
  requests: 240 - i * 35,
  prompt_tokens: 260000 - i * 30000,
  completion_tokens: 140000 - i * 18000,
  cached_tokens: 56000 - i * 5000,
  total_tokens: 456000 - i * 53000,
}));
const byModel = [
  { model: 'gpt-5-codex', requests: 420, prompt_tokens: 680000, completion_tokens: 310000, cached_tokens: 180000, total_tokens: 1170000 },
  { model: 'gpt-5-mini', requests: 260, prompt_tokens: 280000, completion_tokens: 150000, cached_tokens: 74000, total_tokens: 504000 },
  { model: 'claude-sonnet-4', requests: 120, prompt_tokens: 170000, completion_tokens: 96000, cached_tokens: 12000, total_tokens: 278000 },
];
const system = {
  supported: true,
  uptime_seconds: 86400 * 3 + 4200,
  cpu: { usage_pct: 38, cores: 4, load1: 1.24 },
  mem: { used_pct: 62, used_kb: 1515520, total_kb: 2457600 },
  disk: { path: '/', used_pct: 54, free_bytes: 48_000_000_000, used_bytes: 56_000_000_000, total_bytes: 104_000_000_000 },
  go: { goroutines: 96, sys_bytes: 188_000_000 },
  registration: {
    total_rss_kb: 524288,
    node: 2,
    chrome: 3,
    xvfb: 1,
    procs: [
      { pid: 4221, comm: 'node', kind: 'node', rss_kb: 122880 },
      { pid: 4320, comm: 'chrome', kind: 'chrome', rss_kb: 180224 },
      { pid: 4399, comm: 'Xvfb', kind: 'xvfb', rss_kb: 32768 },
    ],
  },
  supervisor_modules: [
    { name: 'registration-scheduler', status: 'running', restart_count: 1, panic_count: 0, unexpected_exit_count: 0, uptime_millis: 3600000, last_message: 'healthy' },
    { name: 'curl-cffi-sidecar', status: 'running', restart_count: 0, panic_count: 0, unexpected_exit_count: 0, uptime_millis: 7200000, last_message: 'listening' },
  ],
  supervisor_events: [
    { time_unix: now - 900, module: 'registration-scheduler', type: 'event', message: 'batch completed', uptime_millis: 3600000 },
  ],
};

function startServer() {
  return spawn(process.execPath, [viteBin, '--host', '127.0.0.1', '--port', String(port), '--strictPort'], {
    cwd: webRoot,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
}

function waitForServer(child) {
  return new Promise((resolve, reject) => {
    let settled = false;
    const timeout = setTimeout(() => finish(reject, new Error('Vite server did not become ready in time')), 30000);
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
    const onData = (chunk) => {
      if (serverReadyPattern.test(String(chunk))) finish(resolve);
    };
    const onExit = (code) => finish(reject, new Error(`Vite server exited before ready: ${code}`));
    child.stdout.on('data', onData);
    child.stderr.on('data', onData);
    child.on('exit', onExit);
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

function ok(body) {
  return { status: 200, contentType: 'application/json', body: JSON.stringify(body) };
}

function settingFields() {
  return [
    { category: 'Gateway', key: 'bind_addr', label: '监听地址', help: 'HTTP 服务监听地址', type: 'string', value: ':8799' },
    { category: 'Gateway', key: 'default_model', label: '默认模型', help: '下游未指定模型时使用', type: 'string', value: 'gpt-5-codex' },
    { category: 'Security', key: 'admin_token_required', label: '管理员 Token', help: '控制台管理入口保护', type: 'bool', value: true },
    { category: 'Limits', key: 'max_concurrency', label: '最大并发', help: '全局调度并发上限', type: 'int', value: 32 },
  ];
}

function settingsCenterResponse(url) {
  const sections = new URL(url).searchParams.get('sections') || '';
  if (sections.includes('automation')) {
    return { automation: {
      policies: [
        { type: 'refill', enabled: true, config: { target: 20, threshold: 5, interval: 300, identity_mode: 'email', register_method: 'protocol_v2', group: 'cyber', egress: 'direct' } },
        { type: 'health', enabled: true, config: { interval: 600 } },
      ],
      readiness: { ready: true, blockers: [] },
      stats: { active_jobs: 1, completed_today: 8 },
    }};
  }
  if (sections.includes('registrar')) {
    return { registrar: { heroSmsService: 'dr', heroSmsCountry: 73, mailProvider: '1secmail', mailDomains: ['guerrillamail.com', 'sharklasers.com'], proxyHost: 'br.proxy.local', proxyPort: 3010 } };
  }
  if (sections.includes('lifecycle')) {
    return { lifecycle: { defaults: { sms: 'herosms', mailbox: 'cloudflare', captcha: 'yescaptcha', group: 'cyber', egress: 'direct' } } };
  }
  if (sections.includes('logging')) {
    return { logging: { verbose_logging: true, failure_threshold: 0.6, log_retention_days: 14, degraded: false } };
  }
  if (sections.includes('memory')) {
    return { memory: { lifecycle_batch_size: 100, lifecycle_concurrency: 5, go_memory_limit_mb: 1024, reg_combined_output_cap: 1048576 } };
  }
  return {};
}

function mockPayload(url, role) {
  const { pathname } = new URL(url);
  if (pathname === '/auth/me') {
    if (!role) return { authed: false };
    return { authed: true, via: 'session', role, email: role === 'admin' ? 'admin@example.com' : 'user@example.com', name: role === 'admin' ? 'Admin Operator' : 'User One' };
  }
  if (pathname === '/healthz') return { ok: true, status: 'ok' };
  if (pathname === '/admin/accounts/summary') return { total: 128, active: 96, quarantined: 8, cooling: 18, recheck: 6, codex: 90, claude: 28, other: 10 };
  if (pathname === '/admin/accounts') return { accounts, total: accounts.length };
  if (pathname === '/admin/groups') return { groups };
  if (pathname === '/admin/egress-profiles') return { profiles: egresses };
  if (pathname === '/admin/providers') return { providers };
  if (pathname === '/admin/users') return { users };
  if (pathname === '/admin/api-keys') return { keys: apiKeys };
  if (pathname === '/admin/usage') return { usage: usageRows };
  if (pathname === '/admin/usage/timeseries') return { buckets };
  if (pathname === '/admin/usage/by-model') return { models: byModel };
  if (pathname === '/admin/register/stats') return { totals: { succeeded: 48, failed: 4, success_rate: 0.923 }, by_day: Array.from({ length: 8 }, (_, i) => ({ date: `2026-06-${String(22 + i).padStart(2, '0')}`, succeeded: 4 + i, failed: i % 2 })) };
  if (pathname === '/admin/system') return system;
  if (pathname === '/admin/register/batch') return { jobs: [
    { id: 'job_001', method: 'protocol_v2', total: 12, succeeded: 9, failed: 1, status: 'running' },
    { id: 'job_000', method: 'node', total: 8, succeeded: 8, failed: 0, status: 'completed' },
  ] };
  if (pathname === '/admin/register/readiness') return { ready: true, providers: { sms: 1, mailbox: 1, email_otp: 1 }, blockers: [] };
  if (pathname === '/admin/register/countries') return [
    { isoCode: 'BR', nameZh: '巴西', name: 'Brazil' },
    { isoCode: 'US', nameZh: '美国', name: 'United States' },
    { isoCode: 'PL', nameZh: '波兰', name: 'Poland' },
  ];
  if (pathname === '/admin/register/providers/options') return {
    sms: [{ label: 'HeroSMS', value: 'herosms' }],
    mailbox: [{ label: 'Cloudflare Mailbox', value: 'cloudflare' }],
    captcha: [{ label: 'YesCaptcha', value: 'yescaptcha' }],
  };
  if (pathname === '/admin/lifecycle/tasks') return { tasks: [
    { id: 'life_001', task_type: 'register', platform: 'chatgpt', group_name: 'cyber', egress_id: 'direct', status: 'running', target_count: 20, completed_count: 11, created_at: now - 1800 },
    { id: 'life_000', task_type: 'health', platform: 'chatgpt', group_name: 'staging', egress_id: 'br-residential', status: 'completed', target_count: 50, completed_count: 50, created_at: now - 7200 },
  ] };
  if (pathname === '/admin/lifecycle/services') return { scheduler: { name: 'scheduler', status: 'running' }, sidecar: { name: 'sidecar', status: 'running' } };
  if (pathname === '/admin/quota') return { quota: accounts.map((account, i) => ({ account_id: account.id, label: account.label, provider: account.provider, used_percent: 45 + i * 18, secondary_7d_used_pct: 25 + i * 14, remaining_tokens: 1_200_000 - i * 200_000, status: i === 2 ? 'warning' : 'ok', reset_at: now + (i + 1) * 3600 })) };
  if (pathname === '/admin/cf-events') return { events: [
    { id: 'cf_1', created_at: now - 600, account_id: 'acct_prod_001', egress_id: 'direct', status: 403, category: 'challenge', cf_ray: '8abc-test', message: 'Managed challenge observed' },
    { id: 'cf_2', created_at: now - 3600, account_id: 'acct_stage_002', egress_id: 'br-residential', status: 200, category: 'pass', cf_ray: '8def-test', message: 'Request passed' },
  ] };
  if (pathname === '/admin/audit') return { rows: [
    { id: 'audit_1', created_at: now - 120, account_label: 'primary-prod', account_id: 'acct_prod_001', action: 'health-test', state: 'alive', reason: '', detail: 'latency=820ms' },
    { id: 'audit_2', created_at: now - 980, account_label: 'staging-worker', account_id: 'acct_stage_002', action: 'quarantine', state: 'rate_limited', reason: '429', detail: 'cooldown applied' },
  ] };
  if (pathname === '/admin/gopay') return { accounts: [
    { id: 'pay_001', email: 'primary@example.com', account_id: 'acct_prod_001', status: 'active', plan: 'plus', amount: 20, created_at: now - 86400, expires_at: now + 86400 * 20 },
    { id: 'pay_002', email: 'stage@example.com', account_id: 'acct_stage_002', status: 'pending', plan: 'team', amount: 30, created_at: now - 3600 },
  ] };
  if (pathname === '/admin/config') return settingFields();
  if (pathname === '/admin/thinking') return { mode: 'medium', budget: 4096, enabled: true };
  if (pathname === '/admin/moderation') return { enabled: true, blocklist: ['abuse', 'fraud'], audit: true };
  if (pathname === '/admin/settings-center') return settingsCenterResponse(url);
  if (pathname === '/user/usage') return byModel;
  if (pathname === '/user/usage/timeseries') return { buckets };
  if (pathname === '/user/api-keys') return apiKeys;
  if (pathname === '/user/profile') return { id: 'user_1', email: 'user@example.com', name: 'User One', role: 'user', status: 'active' };
  return { ok: true };
}

async function installMocks(page, role) {
  const badResponses = [];
  page.on('response', (response) => {
    if (response.status() >= 400) badResponses.push({ status: response.status(), url: response.url() });
  });
  await page.setRequestInterception(true);
  page.on('request', (req) => {
    const url = req.url();
    const { pathname } = new URL(url);
    if (pathname === '/client/errors') {
      req.respond({ status: 204, body: '' });
      return;
    }
    if (pathname === '/admin/oauth/start') {
      req.respond(ok({ session_id: 'oauth_smoke', auth_url: 'https://example.com/oauth/start', expires_in: 900 }));
      return;
    }
    if (pathname.startsWith('/auth/') || pathname.startsWith('/admin/') || pathname.startsWith('/user/') || pathname === '/healthz') {
      req.respond(ok(mockPayload(url, role)));
      return;
    }
    req.continue();
  });
  return badResponses;
}

async function preparePage(browser, theme, role) {
  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 900, deviceScaleFactor: 1 });
  await page.evaluateOnNewDocument((nextTheme) => {
    localStorage.setItem('pool_theme', nextTheme);
    localStorage.setItem('pool_admin_token', 'testadmin_token_local');
  }, theme);
  const badResponses = await installMocks(page, role);
  return { page, badResponses };
}

async function captureRoute(browser, theme, item) {
  const { page, badResponses } = await preparePage(browser, theme, item.role);
  try {
    await page.goto(`${baseURL}${item.route}`, { waitUntil: 'networkidle0', timeout: 60000 });
    await wait(900);
    const file = `${theme}-${item.name}.png`;
    await page.screenshot({ path: path.join(outputDir, file), fullPage: false });
    return { file, badResponses };
  } finally {
    await page.close();
  }
}

async function captureLogin(browser, theme) {
  const { page, badResponses } = await preparePage(browser, theme, '');
  try {
    await page.goto(`${baseURL}/login`, { waitUntil: 'networkidle0', timeout: 60000 });
    await wait(700);
    const file = `${theme}-00-login.png`;
    await page.screenshot({ path: path.join(outputDir, file), fullPage: false });
    return { file, badResponses };
  } finally {
    await page.close();
  }
}

async function captureOAuthModal(browser, theme) {
  const { page, badResponses } = await preparePage(browser, theme, 'admin');
  try {
    await page.goto(`${baseURL}/accounts`, { waitUntil: 'networkidle0', timeout: 60000 });
    await wait(700);
    const clicked = await page.evaluate(() => {
      const button = [...document.querySelectorAll('button')].find((el) => el.textContent.includes('添加账号'));
      if (!button) return false;
      button.click();
      return true;
    });
    if (!clicked) throw new Error('添加账号 button not found');
    await wait(900);
    const file = `${theme}-23-oauth-modal.png`;
    await page.screenshot({ path: path.join(outputDir, file), fullPage: false });
    return { file, badResponses };
  } finally {
    await page.close();
  }
}

async function main() {
  fs.mkdirSync(outputDir, { recursive: true });
  console.log(`Screenshot directory: ${outputDir}`);
  const server = startServer();
  try {
    await waitForServer(server);
    const browser = await puppeteer.launch({ headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'] });
    const results = [];
    try {
      for (const theme of ['light', 'dark']) {
        results.push(await captureLogin(browser, theme));
        for (const item of [...adminRoutes, ...portalRoutes]) {
          results.push(await captureRoute(browser, theme, item));
          console.log(`✓ ${theme}-${item.name}.png`);
        }
        results.push(await captureOAuthModal(browser, theme));
        console.log(`✓ ${theme}-23-oauth-modal.png`);
      }
    } finally {
      await browser.close();
    }
    const bad = results.flatMap((result) => result.badResponses.map((response) => `${result.file}: ${response.status} ${response.url}`));
    if (bad.length) {
      console.error('Screenshots completed, but failed responses were observed:');
      for (const line of bad) console.error(`- ${line}`);
      process.exitCode = 1;
    }
    console.log(`Done. ${results.length} screenshots saved to ${outputDir}`);
  } finally {
    await stopServer(server);
  }
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
