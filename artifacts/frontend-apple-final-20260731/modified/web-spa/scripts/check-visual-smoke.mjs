import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import puppeteer from 'puppeteer';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workspaceRoot = path.resolve(root, '..');
const screenshotDir = path.join(workspaceRoot, '.run', 'screenshots');
const port = Number(process.env.VISUAL_SMOKE_PORT || 5191);
const baseURL = `http://127.0.0.1:${port}/console`;
const serverReadyPattern = /Local:\s+http:\/\/127\.0\.0\.1:/;
const viteBin = path.join(root, 'node_modules', 'vite', 'bin', 'vite.js');

const apiKeyRows = [
  {
    label: 'production-cli',
    key_type: 'inference',
    user_group_id: 'ug_production',
    group_name: '',
    force_model: 'gpt-5-codex',
    force_effort: 'medium',
    secret: '',
    enabled: true,
    key_hash: 'hash_prod_abcdef1234567890',
    created_at: Math.floor(Date.now() / 1000) - 3600,
  },
  {
    label: 'account-import',
    key_type: 'pool_import',
    user_group_id: '',
    group_name: 'staging',
    force_model: '',
    force_effort: '',
    secret: '',
    enabled: false,
    key_hash: 'hash_qa_1234567890abcdef',
    created_at: Math.floor(Date.now() / 1000) - 7200,
  },
];
const accountRows = [
  {
    id: 'acct_prod_001',
    label: 'primary-prod',
    email: 'primary@example.com',
    provider: 'codex',
    group_name: 'cyber',
    plan_type: 'pro',
    status: 'active',
  },
  {
    id: 'acct_stage_002',
    label: 'staging-worker',
    email: 'stage@example.com',
    provider: 'custom',
    group_name: 'staging',
    plan_type: 'team',
    status: 'disabled',
    quarantine_until: Math.floor(Date.now() / 1000) + 3600,
  },
];
const groupRows = [
  { name: 'cyber', force_model: 'gpt-5-codex', force_effort: 'medium' },
  { name: 'staging', force_model: '', force_effort: '' },
];
const userGroupRows = [
  { id: 'ug_production', name: 'Production users', targets: [{ kind: 'account_pool_group', id: 'cyber' }] },
  { id: 'ug_staging', name: 'Staging users', targets: [{ kind: 'account_pool_group', id: 'staging' }] },
];
const providerRows = [
  {
    id: 'openai',
    name: 'OpenAI',
    base_url: 'https://api.openai.com/v1',
    enabled: true,
    auto_discover_models: true,
    models: ['gpt-5-codex', 'gpt-5-mini'],
  },
  {
    id: 'edge-lab',
    name: 'Edge Lab',
    base_url: 'https://edge.example.com/v1',
    enabled: false,
    auto_discover_models: false,
    models: ['custom-chat'],
  },
];
const egressRows = [
  {
    id: 'egress_direct',
    name: 'Direct',
    type: 'direct',
    health: 'healthy',
    stream_capable: true,
  },
  {
    id: 'egress_alt',
    name: 'Alt Proxy',
    type: 'curl_cffi_sidecar',
    health: 'healthy',
    stream_capable: true,
  },
];
const userRows = [
  {
    id: 'user_alice',
    email: 'alice@example.com',
    name: 'Alice Admin',
    role: 'admin',
    status: 'active',
    created_at: Math.floor(Date.now() / 1000) - 86400,
  },
  {
    id: 'user_bob',
    email: 'bob@example.com',
    name: 'Bob User',
    role: 'user',
    status: 'disabled',
    created_at: Math.floor(Date.now() / 1000) - 172800,
  },
];

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
      if (serverReadyPattern.test(text)) {
        finish(resolve);
      }
    };
    const onExit = (code) => finish(reject, new Error(`Vite server exited before ready: ${code}`));
    child.stdout.on('data', onData);
    child.stderr.on('data', onData);
    child.on('exit', onExit);
  });
}

function startServer() {
  const child = spawn(
    process.execPath,
    [viteBin, '--host', '127.0.0.1', '--port', String(port), '--strictPort'],
    { cwd: root, stdio: ['ignore', 'pipe', 'pipe'] },
  );
  return child;
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

function json(body) {
  return { status: 200, contentType: 'application/json', body: JSON.stringify(body) };
}

async function installMocks(page) {
  await page.setRequestInterception(true);
  page.on('request', (req) => {
    const requestPath = new URL(req.url()).pathname;
    if (requestPath === '/auth/me') {
      req.respond(json({ authed: true, via: 'session', role: 'admin', email: 'admin@example.com', name: 'Admin Operator' }));
      return;
    }
    if (requestPath === '/admin/api-keys') {
      req.respond(json({ keys: apiKeyRows }));
      return;
    }
    if (requestPath === '/admin/accounts') {
      req.respond(json({ accounts: accountRows, total: accountRows.length }));
      return;
    }
    if (requestPath === '/admin/groups') {
      req.respond(json({ groups: groupRows }));
      return;
    }
    if (requestPath === '/admin/user-groups') {
      req.respond(json(userGroupRows));
      return;
    }
    if (requestPath === '/admin/providers') {
      req.respond(json({ providers: providerRows }));
      return;
    }
    if (requestPath === '/admin/egress-profiles') {
      req.respond(json({ profiles: egressRows }));
      return;
    }
    if (requestPath === '/admin/users') {
      req.respond(json({ users: userRows }));
      return;
    }
    if (requestPath === '/client/errors') {
      req.respond({ status: 204, body: '' });
      return;
    }
    req.continue();
  });
}

async function runCase(browser, testCase) {
  const page = await browser.newPage();
  const badResponses = [];
  page.on('response', (response) => {
    if (response.status() >= 400) {
      badResponses.push({ status: response.status(), url: response.url() });
    }
  });
  await page.setViewport({
    width: testCase.width,
    height: testCase.height,
    deviceScaleFactor: testCase.width < 600 ? 2 : 1,
    isMobile: testCase.width < 600,
  });
  if (testCase.reducedMotion) {
    await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }]);
  }
  await installMocks(page);
  await page.goto(`${baseURL}${testCase.route}`, { waitUntil: 'networkidle0', timeout: 60000 });
  await page.screenshot({ path: testCase.screenshot, fullPage: true });
  const metrics = await page.evaluate(({ expectedMobileHeader, expectedMobileCards, requiredText, requiredLabels }) => {
    const viewport = { width: window.innerWidth, height: window.innerHeight };
    const text = document.body.textContent.replace(/\s+/g, ' ').trim();
    const headers = [...document.querySelectorAll('.pool-table-wrapper th')].map((el) => el.textContent.trim()).filter(Boolean);
    const labels = [...document.querySelectorAll('[aria-label]')].map((el) => el.getAttribute('aria-label') || '');
    const rect = (selector) => {
      const el = document.querySelector(selector);
      if (!el) return null;
      const r = el.getBoundingClientRect();
      return { left: r.left, top: r.top, right: r.right, bottom: r.bottom, width: r.width, height: r.height };
    };
    const sider = rect('.pool-sider');
    const topTitle = rect('.pool-topbar-title');
    const topActions = rect('.pool-topbar-actions');
    const mobileLists = [...document.querySelectorAll('.pool-mobile-list[role="list"]')];
    const mobileListItems = [...document.querySelectorAll('.pool-mobile-list[role="list"] > [role="listitem"]')];
    return {
      documentWidth: document.documentElement.scrollWidth,
      noPageOverflow: document.documentElement.scrollWidth <= viewport.width + 1,
      topbarOverlap: !!(topTitle && topActions && topTitle.right > topActions.left - 1 && topTitle.bottom > topActions.top && topTitle.top < topActions.bottom),
      siderHidden: viewport.width >= 768 ? true : !!sider && sider.right <= 1,
      headers,
      hasDesktopColumns: headers.includes('Key / 一键安装') && headers.includes('操作'),
      hasExpectedMobileHeader: !expectedMobileHeader || (headers.length === 1 && headers[0] === expectedMobileHeader),
      hasExpectedMobileCards: !expectedMobileCards || (
        headers.length === 0
        && mobileLists.length === 1
        && mobileListItems.length > 0
      ),
      hasRequiredText: requiredText.every((item) => text.includes(item)),
      hasRequiredLabels: requiredLabels.every((item) => labels.includes(item)),
      reducedMotion: window.matchMedia('(prefers-reduced-motion: reduce)').matches,
    };
  }, {
    expectedMobileHeader: testCase.expectedMobileHeader || '',
    expectedMobileCards: Boolean(testCase.expectedMobileCards),
    requiredText: testCase.requiredText || [],
    requiredLabels: testCase.requiredLabels || [],
  });
  await page.close();
  return { name: testCase.name, badResponses, metrics, screenshot: testCase.screenshot };
}

function assertCase(result) {
  const failures = [];
  if (result.badResponses.length > 0) {
    failures.push(`has failed resources: ${JSON.stringify(result.badResponses)}`);
  }
  if (!result.metrics.noPageOverflow) failures.push(`document overflows horizontally (${result.metrics.documentWidth}px)`);
  if (result.metrics.topbarOverlap) failures.push('topbar title overlaps actions');
  if (!result.metrics.siderHidden) failures.push('mobile sidebar is visible while closed');
  if (result.name === 'desktop-keys' && !result.metrics.hasDesktopColumns) failures.push('desktop table columns are missing');
  if (result.name.startsWith('mobile-') && !result.metrics.hasExpectedMobileHeader) failures.push('mobile table is not using the expected single-column layout');
  if (result.name.startsWith('mobile-') && !result.metrics.hasExpectedMobileCards) failures.push('mobile card list is missing');
  if (result.name.startsWith('mobile-') && !result.metrics.hasRequiredText) failures.push('mobile page is missing required row text/actions');
  if (result.name.startsWith('mobile-') && !result.metrics.hasRequiredLabels) failures.push('mobile page is missing required accessible action labels');
  if (result.name === 'mobile-keys' && !result.metrics.reducedMotion) failures.push('reduced-motion media query is not active');
  return failures;
}

async function main() {
  fs.mkdirSync(screenshotDir, { recursive: true });
  const server = startServer();
  try {
    await waitForServer(server);
    const browser = await puppeteer.launch({ headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'] });
    try {
      const results = [];
      for (const testCase of [
        { name: 'desktop-keys', route: '/keys', width: 1440, height: 900, reducedMotion: false, screenshot: path.join(screenshotDir, 'accept-desktop-keys.png') },
        {
          name: 'mobile-keys',
          route: '/keys',
          width: 390,
          height: 844,
          reducedMotion: true,
          expectedMobileHeader: 'API Key',
          requiredText: ['旧 Key，需轮换后复制'],
          requiredLabels: ['API Key 操作'],
          screenshot: path.join(screenshotDir, 'accept-mobile-keys.png'),
        },
        {
          name: 'mobile-accounts',
          route: '/accounts',
          width: 390,
          height: 844,
          reducedMotion: true,
          expectedMobileCards: true,
          requiredText: ['primary-prod'],
          requiredLabels: ['账号操作'],
          screenshot: path.join(screenshotDir, 'accept-mobile-accounts.png'),
        },
        {
          name: 'mobile-providers',
          route: '/providers',
          width: 390,
          height: 844,
          reducedMotion: true,
          expectedMobileHeader: '提供商',
          requiredText: ['OpenAI'],
          requiredLabels: ['提供商操作'],
          screenshot: path.join(screenshotDir, 'accept-mobile-providers.png'),
        },
        {
          name: 'mobile-users',
          route: '/users',
          width: 390,
          height: 844,
          reducedMotion: true,
          expectedMobileHeader: '用户',
          requiredText: ['alice@example.com'],
          requiredLabels: ['用户操作'],
          screenshot: path.join(screenshotDir, 'accept-mobile-users.png'),
        },
      ]) {
        results.push(await runCase(browser, testCase));
      }
      const failures = results.flatMap((result) => assertCase(result).map((failure) => `${result.name}: ${failure}`));
      if (failures.length > 0) {
        console.error('Visual smoke check failed:');
        for (const failure of failures) console.error(`- ${failure}`);
        console.error(JSON.stringify(results, null, 2));
        process.exit(1);
      }
      console.log('Visual smoke check passed.');
      for (const result of results) {
        console.log(`- ${result.name}: ${path.relative(workspaceRoot, result.screenshot)}`);
      }
    } finally {
      await browser?.close?.();
    }
  } finally {
    await stopServer(server);
  }
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
