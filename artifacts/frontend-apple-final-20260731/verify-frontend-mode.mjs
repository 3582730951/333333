import fs from 'node:fs';
import puppeteer from 'puppeteer-core';

const mode = process.argv[2];
if (!['baseline', 'modified'].includes(mode)) throw new Error('usage: verify-frontend-mode.mjs baseline|modified');

const root = '/root/autodl-tmp/frontend-ui-shot-20260731';
const base = 'http://127.0.0.1:34274';
const config = JSON.parse(fs.readFileSync(`${root}/runtime/config.json`, 'utf8'));
const browser = await puppeteer.launch({
  executablePath: '/root/.cache/puppeteer/chrome/linux-150.0.7871.24/chrome-linux64/chrome',
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu', '--lang=zh-CN'],
});

const results = {};
for (const [name, route] of [
  ['settings', '/settings-v2?tab=config'],
  ['system', '/system'],
  ['accounts', '/accounts'],
]) {
  const page = await browser.newPage();
  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
  await page.emulateMediaFeatures([{ name: 'prefers-reduced-motion', value: 'reduce' }]);
  await page.evaluateOnNewDocument((token) => {
    localStorage.setItem('pool_admin_token', token);
    localStorage.setItem('pool_theme', 'light');
    localStorage.setItem('pool_locale', 'zh');
  }, config.admin_token);
  const failures = [];
  const consoleErrors = [];
  page.on('response', (response) => {
    if (response.status() >= 400 && response.url().startsWith(base)) failures.push(`${response.status()} ${response.url()}`);
  });
  page.on('console', (message) => {
    if (message.type() === 'error') consoleErrors.push(message.text());
  });
  page.on('pageerror', (error) => consoleErrors.push(error.message));
  await page.goto(`${base}/console${route}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  await page.waitForSelector('[data-page-ready="true"]', { timeout: 30_000 });
  await new Promise((resolve) => setTimeout(resolve, 900));
  results[name] = await page.evaluate(() => {
    const visible = (node) => {
      if (!node) return false;
      const rect = node.getBoundingClientRect();
      const style = getComputedStyle(node);
      return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none';
    };
    const categories = [...document.querySelectorAll('.pool-settings-category')];
    const quota = [...document.querySelectorAll('[role="progressbar"]')]
      .find((node) => (node.getAttribute('aria-label') || '').includes('已用 90%'));
    return {
      height: document.body.scrollHeight,
      overflow: document.body.scrollWidth > document.body.clientWidth + 1,
      categories: categories.length,
      expandedCategories: categories.filter((node) => node.getAttribute('data-expanded') === 'true').length,
      compactRecords: [...document.querySelectorAll('.pool-compact-record')].filter(visible).length,
      visibleTables: [...document.querySelectorAll('table')].filter(visible).length,
      mobileLists: [...document.querySelectorAll('.pool-mobile-list[role="list"]')].filter(visible).length,
      quota90: quota?.getAttribute('aria-valuenow') === '90',
      alerts: [...document.querySelectorAll('[role="alert"], .pool-load-error, .pool-error-banner')].filter(visible).length,
    };
  });
  results[name].failedResponses = failures;
  results[name].consoleErrors = consoleErrors;
  await page.close();
}

await browser.close();
const common = Object.values(results).every((result) => (
  !result.overflow
  && result.alerts === 0
  && result.failedResponses.length === 0
  && result.consoleErrors.length === 0
));
const behavior = mode === 'baseline'
  ? results.settings.categories === 0
    && results.settings.height > 5_000
    && results.system.compactRecords === 0
    && results.system.visibleTables >= 2
    && results.system.height > 6_000
  : results.settings.categories >= 4
    && results.settings.expandedCategories === 1
    && results.settings.height < 1_850
    && results.system.compactRecords >= 2
    && results.system.visibleTables === 0
    && results.system.height < 6_500
    && results.accounts.mobileLists >= 1
    && results.accounts.quota90;
const report = {
  mode,
  ok: common && behavior,
  common,
  behavior,
  results,
};
fs.writeFileSync(`${root}/artifacts/frontend-apple-final/${mode}-behavior.json`, `${JSON.stringify(report, null, 2)}\n`);
console.log(JSON.stringify(report, null, 2));
if (!report.ok) process.exitCode = 1;
