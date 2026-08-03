#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import puppeteer from 'puppeteer';

function routeManifestFromSource() {
  const source = fs.readFileSync(new URL('../src/app/routeDefinitions.ts', import.meta.url), 'utf8');
  const slice = (start, end) => {
    const from = source.indexOf(start);
    const to = source.indexOf(end, from + start.length);
    if (from < 0 || to < 0) throw new Error(`route manifest block missing: ${start}`);
    return source.slice(from, to);
  };
  const paths = (block) => [...block.matchAll(/\bpath:\s*'([^']+)'/g)].map((match) => match[1]);
  return {
    admin: [...new Set([
      ...paths(slice('export const adminRoutes', 'export const portalRoutes')),
      ...paths(slice('export const legacyRedirects', 'export const settingsSections')),
    ])],
    portal: [...new Set(paths(slice('export const portalRoutes', 'export const legacyRedirects')))],
  };
}

function configuredViewport() {
  const match = String(process.env.POOL_AUDIT_VIEWPORT || '1440x900').match(/^(\d+)x(\d+)$/);
  if (!match) throw new Error('POOL_AUDIT_VIEWPORT must look like 1440x900');
  return { width: Number(match[1]), height: Number(match[2]) };
}

const baseURL = new URL(process.env.POOL_AUDIT_BASE_URL || 'http://127.0.0.1:8799');
baseURL.pathname = baseURL.pathname.replace(/\/$/, '');
const outputPath = path.resolve(process.env.POOL_AUDIT_OUTPUT || '.run/live-fullstack-audit.json');
fs.mkdirSync(path.dirname(outputPath), { recursive: true });

const healthResponse = await fetch(new URL('/healthz', baseURL));
if (!healthResponse.ok) throw new Error(`live backend health check returned HTTP ${healthResponse.status}`);

const browser = await puppeteer.launch({
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
});
await browser.defaultBrowserContext().overridePermissions(`${baseURL.protocol}//${baseURL.host}`, [
  // Puppeteer maps sanitized-write to Chrome's user-gesture clipboard grant;
  // clipboard-write maps to the stricter read/write permission and is denied by
  // current headless Chromium even after overridePermissions.
  'clipboard-read', 'clipboard-sanitized-write',
]);
const page = await browser.newPage();
await page.setViewport(configuredViewport());
const adminToken = String(process.env.POOL_AUDIT_ADMIN_TOKEN || '').trim();
if (adminToken) {
  await page.evaluateOnNewDocument((token) => localStorage.setItem('pool_admin_token', token), adminToken);
}

const current = { route: '' };
const runtimeErrors = [];
const serverErrors = [];
page.on('pageerror', (error) => runtimeErrors.push({ route: current.route, type: 'pageerror', message: error.message }));
page.on('console', (message) => {
  if (message.type() === 'error') runtimeErrors.push({ route: current.route, type: 'console.error', message: message.text() });
});
page.on('response', (response) => {
  if (response.status() >= 500) serverErrors.push({ route: current.route, status: response.status(), url: response.url() });
});

const results = [];
const routeManifest = routeManifestFromSource();
const auditRoute = async (route, role) => {
  current.route = `${role}:${route}`;
  const started = Date.now();
  let navigationError = '';
  try {
    const target = new URL(`/console${route}`, baseURL);
    await page.goto(target.href, { waitUntil: 'domcontentloaded', timeout: 30_000 });
    await page.waitForFunction(
      () => document.querySelector('[data-page-ready="true"]')
        || document.querySelector('.pool-error-boundary')
        || document.querySelector('[role="alert"]'),
      { timeout: 30_000 },
    );
  } catch (error) {
    navigationError = String(error?.message || error);
  }
  const state = await page.evaluate(() => ({
    ready: Boolean(document.querySelector('[data-page-ready="true"]')),
    errorBoundary: Boolean(document.querySelector('.pool-error-boundary')),
    alert: document.querySelector('[role="alert"]')?.textContent?.trim().slice(0, 240) || '',
    overflow: document.documentElement.scrollWidth > window.innerWidth,
    title: document.querySelector('h1,h2')?.textContent?.trim() || '',
  }));
  results.push({ role, route, duration_ms: Date.now() - started, navigationError, ...state });
};
let oauth = { tested: false, passed: false, error: '' };
let portal = { tested: false, passed: false, routes: 0, error: '' };
try {
  for (const route of routeManifest.admin) await auditRoute(route, 'admin');

  if (process.env.POOL_AUDIT_SKIP_OAUTH !== '1') {
    current.route = '/accounts#oauth';
    oauth.tested = true;
    try {
      await page.goto(new URL('/console/accounts', baseURL).href, { waitUntil: 'domcontentloaded', timeout: 30_000 });
      await page.waitForSelector('[data-page-ready="true"]', { timeout: 30_000 });
      const clickButtonWithText = async (label) => {
        const clicked = await page.evaluate((text) => {
          const button = [...document.querySelectorAll('button')]
            .find((candidate) => candidate.textContent?.trim() === text);
          if (!button) return false;
          button.click();
          return true;
        }, label);
        if (!clicked) throw new Error(`button not found: ${label}`);
      };
      await clickButtonWithText('添加账号');
      await clickButtonWithText('生成授权链接');
      await page.waitForSelector('input[aria-label="授权链接"]', { timeout: 30_000 });
      const authURL = await page.$eval('input[aria-label="授权链接"]', (input) => input.value);
      const parsed = new URL(authURL);
      await page.click('button[aria-label="复制授权链接"]');
      await page.waitForSelector('button[aria-label="授权链接已复制"]', { timeout: 10_000 });
      const copied = await page.evaluate(() => navigator.clipboard.readText());
      oauth = {
        tested: true,
        passed: copied === authURL && ['http:', 'https:'].includes(parsed.protocol)
          && Boolean(parsed.hostname) && Boolean(parsed.searchParams.get('state'))
          && Boolean(parsed.searchParams.get('client_id')),
        protocol: parsed.protocol,
        host: parsed.host,
        has_state: Boolean(parsed.searchParams.get('state')),
        has_client_id: Boolean(parsed.searchParams.get('client_id')),
        clipboard_match: copied === authURL,
        error: '',
      };
    } catch (error) {
      oauth = { tested: true, passed: false, error: String(error?.message || error) };
    }
  }

  const portalEmail = String(process.env.POOL_AUDIT_USER_EMAIL || '').trim();
  const portalPassword = String(process.env.POOL_AUDIT_USER_PASSWORD || '');
  if (portalEmail && portalPassword) {
    portal.tested = true;
    try {
      current.route = 'user:login';
      const loginResult = await page.evaluate(async ({ email, password }) => {
        localStorage.removeItem('pool_admin_token');
        const response = await fetch('/auth/login', {
          method: 'POST', credentials: 'include', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ email, password }),
        });
        return { ok: response.ok, status: response.status };
      }, { email: portalEmail, password: portalPassword });
      if (!loginResult.ok) throw new Error(`portal login returned HTTP ${loginResult.status}`);
      const before = results.length;
      for (const route of routeManifest.portal) await auditRoute(route, 'user');
      const portalResults = results.slice(before);
      portal = {
        tested: true,
        passed: portalResults.every((row) => !row.navigationError && row.ready && !row.errorBoundary && !row.overflow),
        routes: portalResults.length,
        error: '',
      };
    } catch (error) {
      portal = { tested: true, passed: false, routes: 0, error: String(error?.message || error) };
    }
  }
} finally {
  await browser.close();
}

const failedRoutes = results.filter((row) => row.navigationError || !row.ready || row.errorBoundary || row.overflow);
const report = {
  timestamp: new Date().toISOString(),
  base_url: `${baseURL.protocol}//${baseURL.host}`,
  viewport: configuredViewport(),
  summary: {
    routes: results.length,
    passed: results.length - failedRoutes.length,
    failed: failedRoutes.length,
    runtime_errors: runtimeErrors.length,
    server_errors: serverErrors.length,
    oauth_link_copy: oauth.tested ? (oauth.passed ? 'passed' : 'failed') : 'skipped',
    portal_routes: portal.tested ? (portal.passed ? 'passed' : 'failed') : 'skipped',
  },
  results,
  runtime_errors: runtimeErrors,
  server_errors: serverErrors,
  oauth,
  portal,
};
fs.writeFileSync(outputPath, `${JSON.stringify(report, null, 2)}\n`, { mode: 0o600 });
console.log(`Live full-stack audit: ${report.summary.passed}/${report.summary.routes} routes passed; report=${outputPath}`);
if (failedRoutes.length || runtimeErrors.length || serverErrors.length
  || (oauth.tested && !oauth.passed) || (portal.tested && !portal.passed)) process.exitCode = 1;
