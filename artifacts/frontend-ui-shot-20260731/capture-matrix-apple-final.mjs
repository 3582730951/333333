import fs from 'node:fs';
import { execFileSync } from 'node:child_process';
import puppeteer from 'puppeteer-core';

const root = '/root/autodl-tmp/frontend-ui-shot-20260731';
const base = 'http://127.0.0.1:34274';
const consoleBase = `${base}/console`;
const config = JSON.parse(fs.readFileSync(`${root}/runtime/config.json`, 'utf8'));
const outputDir = `${root}/screenshots/matrix-apple-final`;
fs.mkdirSync(outputDir, { recursive: true });

const adminRoutes = [
  ['01-dashboard', '/'],
  ['02-accounts', '/accounts'],
  ['03-groups', '/groups'],
  ['04-email-pool', '/email-pool'],
  ['05-providers', '/providers'],
  ['06-models', '/models'],
  ['07-egress', '/egress'],
  ['08-upstream-error-rules', '/upstream-error-rules'],
  ['09-registration', '/registration'],
  ['10-usage', '/usage'],
  ['11-quota', '/quota'],
  ['12-model-quality', '/model-quality'],
  ['13-system', '/system'],
  ['14-cf-events', '/cf-events'],
  ['15-audit', '/audit'],
  ['16-keys', '/keys'],
  ['17-users', '/users'],
  ['18-settings-config', '/settings-v2?tab=config'],
  ['19-settings-automation', '/settings-v2?tab=automation'],
  ['20-settings-registrar', '/settings-v2?tab=registrar'],
  ['21-settings-logging', '/settings-v2?tab=logging'],
  ['22-settings-memory', '/settings-v2?tab=memory'],
  ['23-settings-thinking', '/settings-v2?tab=thinking'],
  ['24-settings-moderation', '/settings-v2?tab=moderation'],
  ['25-ai-chatgpt', '/settings/ai/chatgpt'],
  ['26-ai-claude', '/settings/ai/claude'],
  ['27-ai-kiro', '/settings/ai/kiro'],
  ['28-ai-antigravity', '/settings/ai/antigravity'],
  ['29-ai-codex', '/settings/ai/codex'],
  ['30-ai-claude-code', '/settings/ai/claude-code'],
];
const portalRoutes = [
  ['31-portal-dashboard', '/portal'],
  ['32-portal-keys', '/portal/keys'],
  ['33-portal-models', '/portal/models'],
  ['34-portal-profile', '/portal/profile'],
];
const variants = [
  { name: 'desktop-light', theme: 'light', viewport: { width: 1440, height: 1000, deviceScaleFactor: 1 } },
  { name: 'desktop-dark', theme: 'dark', viewport: { width: 1440, height: 1000, deviceScaleFactor: 1 } },
  { name: 'mobile-light', theme: 'light', viewport: { width: 390, height: 844, deviceScaleFactor: 1 } },
];

const browser = await puppeteer.launch({
  executablePath: '/root/.cache/puppeteer/chrome/linux-150.0.7871.24/chrome-linux64/chrome',
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu', '--lang=zh-CN'],
});
const results = [];

async function configurePage(page, variant, role) {
  await page.setViewport(variant.viewport);
  await page.emulateMediaFeatures([
    { name: 'prefers-color-scheme', value: variant.theme },
    { name: 'prefers-reduced-motion', value: 'reduce' },
  ]);
  await page.evaluateOnNewDocument(({ token, theme, role }) => {
    if (role === 'admin') localStorage.setItem('pool_admin_token', token);
    else localStorage.removeItem('pool_admin_token');
    localStorage.setItem('pool_theme', theme);
    localStorage.setItem('pool_locale', 'zh');
  }, { token: config.admin_token, theme: variant.theme, role });
}

async function capture(context, variant, item, role, options = {}) {
  const [name, route] = item;
  const page = await context.newPage();
  await configurePage(page, variant, role);
  const failedResponses = [];
  const consoleErrors = [];
  page.on('response', (response) => {
    if (response.status() >= 400 && response.url().startsWith(base)) {
      failedResponses.push({ status: response.status(), url: response.url() });
    }
  });
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text());
  });
  page.on('pageerror', (error) => consoleErrors.push(error.message));
  const startedAt = Date.now();
  const file = `${variant.name}-${name}.png`;
  const result = { variant: variant.name, name, route, role, file, ready: false, loadMillis: 0, failedResponses, consoleErrors };
  try {
    await page.goto(`${consoleBase}${route}`, { waitUntil: 'domcontentloaded', timeout: 60000 });
    if (options.login) {
      await page.waitForSelector('input, button', { timeout: 15000 });
    } else {
      await page.waitForSelector('[data-page-ready="true"]', { timeout: 30000 });
      result.ready = true;
    }
    await page.evaluate(async () => { await document.fonts.ready; });
    await new Promise((resolve) => setTimeout(resolve, options.login ? 500 : 900));
    await page.addStyleTag({ content: '*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}' });
    Object.assign(result, await page.evaluate(() => {
      const visible = (node) => {
        const rect = node.getBoundingClientRect();
        const style = getComputedStyle(node);
        return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none';
      };
      const alerts = [...document.querySelectorAll('[role="alert"], .pool-load-error, .pool-alert, .pool-error-banner')]
        .filter(visible).map((node) => node.textContent?.trim()).filter(Boolean);
      const h1 = document.querySelector('h1');
      const h1Range = h1 ? document.createRange() : null;
      if (h1Range && h1) h1Range.selectNodeContents(h1);
      const h1Lines = h1Range ? new Set([...h1Range.getClientRects()].map((rect) => Math.round(rect.top))).size : 0;
      return {
        title: document.title,
        finalURL: location.href,
        h1: [...document.querySelectorAll('h1')].map((node) => node.textContent?.trim()).filter(Boolean),
        h1Lines,
        bodyTextLength: document.body.innerText.length,
        bodyScrollWidth: document.body.scrollWidth,
        bodyClientWidth: document.body.clientWidth,
        bodyScrollHeight: document.body.scrollHeight,
        horizontalOverflow: document.body.scrollWidth > document.body.clientWidth + 1,
        visibleSkeletons: [...document.querySelectorAll('.pool-skel, .pool-skeleton-table, .pool-route-fallback')].filter(visible).length,
        visibleCharts: [...document.querySelectorAll('svg.recharts-surface')].filter(visible).length,
        visibleAlerts: alerts,
      };
    }));
    await page.screenshot({ path: `${outputDir}/${file}`, fullPage: true, type: 'png' });
  } catch (error) {
    result.error = error instanceof Error ? error.message : String(error);
    try { await page.screenshot({ path: `${outputDir}/${file}`, fullPage: true, type: 'png' }); } catch {}
  } finally {
    result.loadMillis = Date.now() - startedAt;
    const expectedLoginProbe = options.login
      && failedResponses.length > 0
      && failedResponses.every((response) => response.status === 401 && response.url.endsWith('/auth/me'))
      && consoleErrors.every((message) => message.includes('401') || message.includes('Failed to load resource'));
    result.expectedLoginProbe = Boolean(expectedLoginProbe);
    result.ok = !result.error && (options.login || result.ready) && !result.horizontalOverflow
      && (options.login || result.h1Lines <= 1)
      && (result.visibleSkeletons || 0) === 0
      && ((failedResponses.length === 0 && consoleErrors.length === 0) || expectedLoginProbe)
      && (result.visibleAlerts?.length || 0) === 0;
    results.push(result);
    console.log(`${result.ok ? 'OK' : 'ISSUE'} ${variant.name} ${name} ${result.loadMillis}ms`);
    await page.close();
  }
}

async function preparePortalContext() {
  const context = await browser.createBrowserContext();
  const page = await context.newPage();
  await page.goto(`${consoleBase}/login`, { waitUntil: 'domcontentloaded', timeout: 60000 });
  const credentials = { email: 'portal.demo.20260731@example.test', password: 'DemoPortal-2026-Strong', name: '演示门户用户' };
  const auth = await page.evaluate(async (credentials) => {
    const request = async (path, body) => {
      const response = await fetch(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, credentials: 'include', body: JSON.stringify(body) });
      return { status: response.status, body: await response.json().catch(() => ({})) };
    };
    let result = await request('/auth/login', credentials);
    if (result.status === 401 || result.status === 404) result = await request('/auth/register', credentials);
    if (result.status >= 400) throw new Error(`portal auth ${result.status}: ${JSON.stringify(result.body)}`);
    const me = await fetch('/auth/me', { credentials: 'include' });
    return me.json();
  }, credentials);
  if (!auth?.id || auth.role !== 'user') throw new Error(`portal identity invalid: ${JSON.stringify(auth)}`);
  execFileSync('sqlite3', [
    `${root}/runtime/pool.sqlite3`,
    `BEGIN IMMEDIATE; UPDATE api_keys SET user_id='${auth.id}' WHERE key_hash LIKE 'ui-demo-key-%'; UPDATE usage_records SET user_id='${auth.id}', api_key_hash='ui-demo-key-production' WHERE route_key_hash LIKE 'ui-demo-%'; COMMIT;`,
  ]);
  await page.close();
  return context;
}

const adminContext = browser.defaultBrowserContext();
for (const variant of variants) {
  for (const item of adminRoutes) await capture(adminContext, variant, item, 'admin');
  await capture(adminContext, variant, ['00-login', '/login'], 'login', { login: true });
}

let portalContext;
try {
  portalContext = await preparePortalContext();
  for (const variant of variants) {
    for (const item of portalRoutes) await capture(portalContext, variant, item, 'user');
  }
} finally {
  if (portalContext) await portalContext.close();
}

await browser.close();
const issues = results.filter((result) => !result.ok);
const summary = {
  capturedAt: new Date().toISOString(),
  base,
  outputDir,
  total: results.length,
  passed: results.length - issues.length,
  issues: issues.length,
  variants: Object.fromEntries(variants.map((variant) => [variant.name, results.filter((result) => result.variant === variant.name).length])),
  results,
};
fs.writeFileSync(`${root}/artifacts/ui-matrix-apple-final-report.json`, `${JSON.stringify(summary, null, 2)}\n`);
const markdown = [
  '# 云端真实 UI 截图矩阵',
  '',
  `- 截图：${summary.total}`,
  `- 通过：${summary.passed}`,
  `- 问题：${summary.issues}`,
  '',
  '## 问题',
  '',
  ...(issues.length ? issues.map((issue) => `- \`${issue.variant}/${issue.name}\`: ${issue.error || `HTTP ${issue.failedResponses.map((x) => x.status).join(',')} console=${issue.consoleErrors.length} overflow=${issue.horizontalOverflow} alerts=${(issue.visibleAlerts || []).join(' | ')}`}`) : ['- 无']),
  '',
];
fs.writeFileSync(`${root}/artifacts/ui-matrix-apple-final-report.md`, markdown.join('\n'));
console.log(`SUMMARY total=${summary.total} passed=${summary.passed} issues=${summary.issues}`);
if (issues.length) process.exitCode = 1;
