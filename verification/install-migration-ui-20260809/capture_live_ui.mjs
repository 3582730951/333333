import fs from 'node:fs';
import path from 'node:path';

import puppeteer from '../../web-spa/node_modules/puppeteer/lib/puppeteer/puppeteer.js';

const baseURL = process.env.POOL_BASE_URL || 'http://127.0.0.1:18787';
const adminToken = process.env.POOL_ADMIN_TOKEN || 'demo-admin-token';
const outputDir = path.resolve('verification/install-migration-ui-20260809/screenshots');

const captures = [
  { name: 'dashboard', route: '/', theme: 'light', width: 1440, height: 900 },
  { name: 'accounts', route: '/accounts', theme: 'light', width: 1440, height: 900 },
  { name: 'usage-dark', route: '/usage', theme: 'dark', width: 1440, height: 900 },
  { name: 'egress', route: '/egress', theme: 'light', width: 1440, height: 900 },
  { name: 'audit', route: '/audit', theme: 'light', width: 1440, height: 900 },
  { name: 'settings', route: '/settings-v2?tab=config', theme: 'light', width: 1440, height: 900 },
  { name: 'settings-claude', route: '/settings-v2?tab=config', theme: 'light', width: 1440, height: 900, search: 'claude' },
  { name: 'ai-claude', route: '/settings/ai/claude', theme: 'light', width: 1440, height: 900 },
  { name: 'dashboard-mobile', route: '/', theme: 'light', width: 390, height: 844, mobile: true },
];

fs.mkdirSync(outputDir, { recursive: true });

const browser = await puppeteer.launch({
  headless: true,
  executablePath: await puppeteer.executablePath(),
  args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage'],
});

const report = [];
try {
  for (const capture of captures) {
    const page = await browser.newPage();
    const browserErrors = [];
    const failedRequests = [];

    page.on('console', (message) => {
      if (message.type() === 'error') browserErrors.push(`console: ${message.text()}`);
    });
    page.on('pageerror', (error) => browserErrors.push(`pageerror: ${error.message}`));
    page.on('requestfailed', (request) => {
      failedRequests.push(`${request.failure()?.errorText || 'failed'} ${request.url()}`);
    });

    await page.setViewport({
      width: capture.width,
      height: capture.height,
      deviceScaleFactor: 1,
      isMobile: Boolean(capture.mobile),
      hasTouch: Boolean(capture.mobile),
    });
    await page.evaluateOnNewDocument((token, theme) => {
      localStorage.setItem('pool_admin_token', token);
      localStorage.setItem('pool_theme', theme);
      localStorage.setItem('pool_locale', 'zh');
    }, adminToken, capture.theme);

    const response = await page.goto(`${baseURL}/console${capture.route}`, {
      waitUntil: 'domcontentloaded',
      timeout: 30_000,
    });

    let pageReady = true;
    try {
      await page.waitForSelector('[data-page-ready="true"]', { timeout: 30_000 });
    } catch {
      pageReady = false;
    }
    if (capture.search) {
      const searchInput = await page.waitForSelector('input[placeholder*="搜索"]', { timeout: 10_000 });
      await searchInput.type(capture.search);
      await new Promise((resolve) => setTimeout(resolve, 300));
    }
    await page.evaluate(async () => {
      if (document.fonts?.ready) await document.fonts.ready;
    });
    await page.addStyleTag({
      content: '*,*::before,*::after{animation-duration:0s!important;transition-duration:0s!important;caret-color:transparent!important}',
    });
    await new Promise((resolve) => setTimeout(resolve, 250));

    const metrics = await page.evaluate(() => ({
      title: document.title,
      heading: document.querySelector('h1,h2')?.textContent?.trim() || '',
      bodyTextLength: document.body?.innerText?.length || 0,
      horizontalOverflow: Math.max(document.documentElement.scrollWidth, document.body?.scrollWidth || 0) > window.innerWidth + 1,
      pathname: window.location.pathname,
      authScreenVisible: Boolean(document.querySelector('input[type="password"], input[placeholder*="Token"]')),
    }));

    const screenshot = path.join(outputDir, `${capture.name}-${capture.width}x${capture.height}.png`);
    await page.screenshot({ path: screenshot, fullPage: true });
    report.push({
      ...capture,
      screenshot,
      httpStatus: response?.status() || 0,
      pageReady,
      ...metrics,
      browserErrors,
      failedRequests,
    });
    await page.close();
  }
} finally {
  await browser.close();
}

const reportPath = path.join(outputDir, 'capture-report.json');
fs.writeFileSync(reportPath, `${JSON.stringify(report, null, 2)}\n`);
console.log(JSON.stringify({ reportPath, captures: report }, null, 2));
