import fs from 'node:fs';
import puppeteer from 'puppeteer-core';

const root = '/root/autodl-tmp/frontend-ui-shot-20260731';
const base = 'http://127.0.0.1:34274';
const consoleBase = `${base}/console`;
const config = JSON.parse(fs.readFileSync(`${root}/runtime/config.json`, 'utf8'));
const outputDir = `${root}/screenshots/apple-v3`;
fs.mkdirSync(outputDir, { recursive: true });

const longIdentity = 'registration-automation-production-owner-with-an-extraordinarily-long-identity@subdomain.example.test';
const routes = [
  ['dashboard', '/'],
  ['accounts', '/accounts'],
  ['email-pool', '/email-pool'],
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

    const metrics = await page.evaluate(({ name, longIdentity, mobile }) => {
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
      const common = {
        h1: h1?.textContent?.trim() || '',
        h1Lines,
        bodyScrollHeight: document.body.scrollHeight,
        bodyScrollWidth: document.body.scrollWidth,
        bodyClientWidth: document.body.clientWidth,
        horizontalOverflow: document.body.scrollWidth > document.body.clientWidth + 1,
        visibleSkeletons: [...document.querySelectorAll('.pool-skel, .pool-skeleton-table, .pool-route-fallback')].filter(visible).length,
        visibleAlerts: alerts,
      };

      if (name === 'dashboard') {
        const subgrid = document.querySelector('.pool-dashboard-command__subgrid');
        const chartFrames = [...document.querySelectorAll('.pool-chart-frame[role="img"]')].filter(visible);
        const topCard = [...document.querySelectorAll('.pool-chart-card')]
          .find((card) => card.querySelector('.t')?.textContent?.trim() === '账号消耗排行');
        const topAccountTicks = topCard
          ? [...topCard.querySelectorAll('.recharts-xAxis .recharts-cartesian-axis-tick-value')].map((node) => node.textContent?.trim()).filter(Boolean)
          : [];
        return {
          ...common,
          visibleCharts: [...document.querySelectorAll('svg.recharts-surface')].filter(visible).length,
          chartFrames: chartFrames.length,
          chartLabels: chartFrames.map((node) => node.getAttribute('aria-label')),
          mobileKpiColumns: mobile && subgrid ? getComputedStyle(subgrid).gridTemplateColumns.split(' ').filter(Boolean).length : null,
          topAccountTicks,
          uniqueTopAccountTicks: new Set(topAccountTicks).size,
        };
      }

      const identityNodes = [...document.querySelectorAll('[aria-label]')]
        .filter((node) => node.getAttribute('aria-label') === longIdentity && visible(node));
      if (name === 'accounts') {
        const quota = [...document.querySelectorAll('[role="progressbar"]')]
          .find((node) => (node.getAttribute('aria-label') || '').includes('已用 90%'));
        return {
          ...common,
          longIdentityVisible: identityNodes.length > 0,
          quotaLabel: quota?.getAttribute('aria-label') || '',
          quotaValue: quota?.getAttribute('aria-valuenow') || '',
          mobileList: Boolean(document.querySelector('.pool-mobile-list[role="list"]')),
          visibleTable: visible(document.querySelector('table')),
          actionCount: [...document.querySelectorAll('.pool-page-actions button')].filter(visible).length,
        };
      }

      const metricCards = [...document.querySelectorAll('.pool-email-metrics .pool-metric-card')].filter(visible);
      return {
        ...common,
        longIdentityVisible: identityNodes.length > 0,
        metricCards: metricCards.length,
        availableMetric: metricCards.find((node) => node.querySelector('.pool-metric-card__label')?.textContent?.trim() === '可用')?.textContent?.trim() || '',
        mobileList: Boolean(document.querySelector('[role="list"][aria-label="邮箱账号列表"]')),
        visibleTable: visible(document.querySelector('table')),
      };
    }, { name, longIdentity, mobile: variant.name.startsWith('mobile') });

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
      ...metrics,
    };
    const commonOK = !result.horizontalOverflow
      && result.h1Lines === 1
      && result.visibleSkeletons === 0
      && result.visibleAlerts.length === 0
      && failedResponses.length === 0
      && consoleErrors.length === 0;
    if (name === 'dashboard') {
      result.ok = commonOK
        && result.visibleCharts >= 3
        && result.chartFrames === result.visibleCharts
        && (!variant.name.startsWith('mobile') || result.mobileKpiColumns === 2)
        && result.uniqueTopAccountTicks === result.topAccountTicks.length;
    } else if (name === 'accounts') {
      result.ok = commonOK
        && result.longIdentityVisible
        && result.quotaValue === '90'
        && (variant.name.startsWith('mobile') ? result.mobileList && !result.visibleTable : result.visibleTable);
    } else {
      result.ok = commonOK
        && result.longIdentityVisible
        && result.metricCards === 5
        && (variant.name.startsWith('mobile') ? result.mobileList && !result.visibleTable : result.visibleTable);
    }
    results.push(result);
    console.log(`${result.ok ? 'OK' : 'ISSUE'} ${variant.name}/${name} height=${result.bodyScrollHeight} h1Lines=${result.h1Lines}`);
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
fs.writeFileSync(`${root}/artifacts/frontend-accounts-dashboard/ui-verification.json`, `${JSON.stringify(report, null, 2)}\n`);
console.log(JSON.stringify({ total: report.total, passed: report.passed, issues: report.issues }, null, 2));
if (issues.length) process.exitCode = 1;
