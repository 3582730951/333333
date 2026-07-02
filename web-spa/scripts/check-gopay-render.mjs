import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

import puppeteer from 'puppeteer';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const workspaceRoot = path.resolve(root, '..');
const port = Number(process.env.GOPAY_RENDER_PORT || 5195);
const baseURL = `http://127.0.0.1:${port}/console`;
const viteBin = path.join(root, 'node_modules', 'vite', 'bin', 'vite.js');
const serverReadyPattern = /Local:\s+http:\/\/127\.0\.0\.1:/;
const now = Math.floor(Date.now() / 1000);

function startServer() {
  return spawn(process.execPath, [viteBin, '--host', '127.0.0.1', '--port', String(port), '--strictPort'], {
    cwd: root,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
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

async function installMocks(page, clientReports) {
  await page.setRequestInterception(true);
  page.on('request', (req) => {
    const { pathname } = new URL(req.url());
    if (pathname === '/auth/me') {
      req.respond(ok({ authed: true, via: 'session', role: 'admin', email: 'admin@example.com', name: 'Admin Operator' }));
      return;
    }
    if (pathname === '/admin/gopay') {
      req.respond(ok({ accounts: [
        { id: 'pay_001', email: 'primary@example.com', account_id: 'acct_prod_001', status: 'active', plan: 'plus', amount: 20, created_at: now - 86400, expires_at: now + 86400 * 20 },
        { id: 'pay_002', email: 'stage@example.com', account_id: 'acct_stage_002', status: 'pending', plan: 'team', amount: 30, created_at: now - 3600 },
      ] }));
      return;
    }
    if (pathname === '/client/errors') {
      clientReports.push(req.postData() || '');
      req.respond({ status: 204, body: '' });
      return;
    }
    if (pathname.startsWith('/admin/') || pathname.startsWith('/user/') || pathname === '/healthz') {
      req.respond(ok({ ok: true }));
      return;
    }
    req.continue();
  });
}

async function main() {
  const screenshotDir = path.join(workspaceRoot, '.run', 'screenshots');
  fs.mkdirSync(screenshotDir, { recursive: true });
  const server = startServer();
  try {
    await waitForServer(server);
    const browser = await puppeteer.launch({ headless: 'new', args: ['--no-sandbox', '--disable-dev-shm-usage'] });
    const page = await browser.newPage();
    const consoleErrors = [];
    const pageErrors = [];
    const clientReports = [];
    const badResponses = [];
    try {
      page.on('console', (msg) => {
        if (['error', 'warning'].includes(msg.type())) consoleErrors.push(`${msg.type()}: ${msg.text()}`);
      });
      page.on('pageerror', (error) => {
        pageErrors.push(error.stack || error.message || String(error));
      });
      page.on('response', (response) => {
        if (response.status() >= 400) badResponses.push(`${response.status()} ${response.url()}`);
      });
      await page.setViewport({ width: 1440, height: 900, deviceScaleFactor: 1 });
      await page.evaluateOnNewDocument(() => {
        localStorage.setItem('pool_theme', 'light');
        localStorage.setItem('pool_admin_token', 'testadmin_token_local');
      });
      await installMocks(page, clientReports);
      await page.goto(`${baseURL}/gopay`, { waitUntil: 'networkidle0', timeout: 60000 });
      await page.waitForSelector('body', { timeout: 10000 });
      await page.screenshot({ path: path.join(screenshotDir, 'gopay-render-check.png'), fullPage: false });
      const metrics = await page.evaluate(() => {
        const text = document.body.textContent.replace(/\s+/g, ' ').trim();
        return {
          hasErrorBoundary: !!document.querySelector('.pool-error-boundary.is-page'),
          hasTable: !!document.querySelector('.pool-gopay-table'),
          hasExpectedRecord: text.includes('primary@example.com') && text.includes('¥20.00'),
          bodyText: text.slice(0, 800),
        };
      });
      const failures = [];
      if (badResponses.length) failures.push(`failed responses: ${badResponses.join('; ')}`);
      if (pageErrors.length) failures.push(`page errors: ${pageErrors.join('\n---\n')}`);
      if (consoleErrors.some((line) => !line.includes('[vite]'))) failures.push(`console errors: ${consoleErrors.join('\n')}`);
      if (clientReports.length) failures.push(`client error reports: ${clientReports.join('\n')}`);
      if (metrics.hasErrorBoundary) failures.push(`Gopay rendered page error boundary. Body: ${metrics.bodyText}`);
      if (!metrics.hasTable || !metrics.hasExpectedRecord) failures.push(`Gopay table did not render expected subscription rows. Body: ${metrics.bodyText}`);
      if (failures.length > 0) {
        console.error('Gopay render check failed:');
        for (const failure of failures) console.error(`- ${failure}`);
        process.exitCode = 1;
      } else {
        console.log('Gopay render check passed.');
      }
    } finally {
      await page.close();
      await browser.close();
    }
  } finally {
    await stopServer(server);
  }
}

main().catch((error) => {
  console.error(error);
  process.exit(1);
});
