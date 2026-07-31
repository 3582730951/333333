import fs from 'node:fs';
import puppeteer from 'puppeteer-core';

const root = '/root/autodl-tmp/frontend-ui-shot-20260731';
const base = 'http://127.0.0.1:34274';
const consoleBase = `${base}/console`;
const config = JSON.parse(fs.readFileSync(`${root}/runtime/config.json`, 'utf8'));
const outputDir = `${root}/screenshots/apple-v4`;
fs.mkdirSync(outputDir, { recursive: true });

const routes = [
  ['registration', '/registration'],
  ['system', '/system'],
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

for (const variant of variants) {
  for (const [name, route] of routes) {
    const page = await browser.newPage();
    await page.setViewport(variant.viewport);
    await page.emulateMediaFeatures([
      { name: 'prefers-color-scheme', value: variant.theme },
      { name: 'prefers-reduced-motion', value: 'reduce' },
    ]);
    await page.evaluateOnNewDocument(({ token, theme }) => {
      localStorage.setItem('pool_admin_token', token);
      localStorage.setItem('pool_theme', theme);
      localStorage.setItem('pool_locale', 'zh');
    }, { token: config.admin_token, theme: variant.theme });

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
    await page.goto(`${consoleBase}${route}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
    await page.waitForSelector('[data-page-ready="true"]', { timeout: 30_000 });
    await page.evaluate(async () => { await document.fonts.ready; });
    await new Promise((resolve) => setTimeout(resolve, 1_000));
    await page.addStyleTag({ content: '*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}' });

    const mobile = variant.name.startsWith('mobile');
    const metrics = await page.evaluate(({ name, mobile }) => {
      const visible = (node) => {
        if (!node) return false;
        const rect = node.getBoundingClientRect();
        const style = getComputedStyle(node);
        return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none';
      };
      const h1 = document.querySelector('h1');
      const h1Range = h1 ? document.createRange() : null;
      if (h1Range && h1) h1Range.selectNodeContents(h1);
      const h1Lines = h1Range ? new Set([...h1Range.getClientRects()].map((rect) => Math.round(rect.top))).size : 0;
      const alerts = [...document.querySelectorAll('[role="alert"], .pool-load-error, .pool-alert, .pool-error-banner')]
        .filter(visible).map((node) => node.textContent?.trim()).filter(Boolean);
      const tables = [...document.querySelectorAll('table')].filter(visible);
      const mobileLists = [...document.querySelectorAll('.pool-mobile-list[role="list"]')].filter(visible);
      const compactRecords = [...document.querySelectorAll('.pool-compact-record')].filter(visible);
      const common = {
        h1: h1?.textContent?.trim() || '',
        h1Lines,
        bodyScrollHeight: document.body.scrollHeight,
        bodyScrollWidth: document.body.scrollWidth,
        bodyClientWidth: document.body.clientWidth,
        horizontalOverflow: document.body.scrollWidth > document.body.clientWidth + 1,
        visibleSkeletons: [...document.querySelectorAll('.pool-skel, .pool-skeleton-table, .pool-route-fallback')].filter(visible).length,
        visibleAlerts: alerts,
        visibleTables: tables.length,
        mobileLists: mobileLists.length,
        compactRecords: compactRecords.length,
      };

      if (name === 'registration') {
        const metricCards = [...document.querySelectorAll('.pool-registration-metrics .pool-metric-card')].filter(visible);
        const formControls = [...document.querySelectorAll('.pool-registration-start-form input, .pool-registration-start-form button')].filter(visible);
        return {
          ...common,
          metricCards: metricCards.length,
          metricColumns: metricCards.length && mobile
            ? getComputedStyle(document.querySelector('.pool-registration-metrics')).gridTemplateColumns.split(' ').filter(Boolean).length
            : null,
          formControls: formControls.length,
          jobCards: [...document.querySelectorAll('.pool-registration-job-card')].filter(visible).length,
        };
      }

      return {
        ...common,
        moduleCards: [...document.querySelectorAll('.pool-system-record')].filter(visible).length,
        systemStats: [...document.querySelectorAll('.pool-stat-grid .pool-stat')].filter(visible).length,
      };
    }, { name, mobile });

    let drawerVerified = null;
    if (mobile) {
      const selector = name === 'registration' ? '.pool-registration-job-card' : '.pool-system-record';
      if (await page.$(selector)) {
        await page.click(selector);
        await page.waitForSelector('.pool-drawer-content', { visible: true, timeout: 5_000 });
        drawerVerified = await page.evaluate((pageName) => ({
          title: document.querySelector('.pool-drawer-title')?.textContent?.trim() || '',
          detailCount: pageName === 'registration'
            ? document.querySelectorAll('.pool-task-detail__grid > div').length
            : document.querySelectorAll('.pool-system-detail__grid > div').length,
        }), name);
        await page.click('.pool-drawer-header button');
      }
    }

    const screenshot = `${variant.name}-${name}.png`;
    await page.screenshot({ path: `${outputDir}/${screenshot}`, fullPage: true, type: 'png' });
    const result = {
      variant: variant.name,
      name,
      route,
      screenshot,
      loadMillis: Date.now() - startedAt,
      failedResponses,
      consoleErrors,
      drawerVerified,
      ...metrics,
    };
    const commonOK = !result.horizontalOverflow
      && result.h1Lines === 1
      && result.visibleSkeletons === 0
      && result.visibleAlerts.length === 0
      && failedResponses.length === 0
      && consoleErrors.length === 0;
    if (name === 'registration') {
      result.ok = commonOK
        && result.metricCards === 4
        && result.formControls >= 3
        && (mobile
          ? result.visibleTables === 0 && result.mobileLists >= 1 && result.jobCards >= 1
            && result.metricColumns === 2 && result.drawerVerified?.detailCount >= 5
          : result.visibleTables >= 1);
    } else {
      result.ok = commonOK
        && result.systemStats === 8
        && (mobile
          ? result.visibleTables === 0 && result.mobileLists >= 2 && result.moduleCards >= 2
            && result.bodyScrollHeight < 6_500 && result.drawerVerified?.detailCount >= 4
          : result.visibleTables >= 2);
    }
    results.push(result);
    console.log(`${result.ok ? 'OK' : 'ISSUE'} ${variant.name}/${name} height=${result.bodyScrollHeight} records=${result.compactRecords}`);
    await page.close();
  }
}

await browser.close();
const issues = results.filter((result) => !result.ok);
const report = {
  capturedAt: new Date().toISOString(),
  base,
  outputDir,
  total: results.length,
  passed: results.length - issues.length,
  issues: issues.length,
  results,
};
fs.writeFileSync(`${root}/artifacts/frontend-registration-system/ui-verification.json`, `${JSON.stringify(report, null, 2)}\n`);
console.log(JSON.stringify({ total: report.total, passed: report.passed, issues: report.issues }, null, 2));
if (issues.length) {
  console.log(JSON.stringify(issues, null, 2));
  process.exitCode = 1;
}
