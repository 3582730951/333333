import fs from 'node:fs';
import puppeteer from 'puppeteer-core';

const root = process.env.ROOT || '/root/autodl-tmp/legacy-install-upgrade-20260731';
const phase = process.env.PHASE || 'old';
const base = process.env.BASE_URL || 'http://127.0.0.1:34276';
const token = process.env.CONFIG_FILE
  ? JSON.parse(fs.readFileSync(process.env.CONFIG_FILE, 'utf8')).admin_token
  : fs.readFileSync(process.env.TOKEN_FILE || `${root}/records/admin.token`, 'utf8').trim();
const outputDir = `${root}/screenshots/${phase}`;
fs.mkdirSync(outputDir, { recursive: true });

const routes = [
  ['dashboard', '/'],
  ['accounts', '/accounts'],
  ['email-pool', '/email-pool'],
  ['registration', '/registration'],
  ['system', '/system'],
  ['settings-config', '/settings-v2?tab=config'],
];

const browser = await puppeteer.launch({
  executablePath: '/root/.cache/puppeteer/chrome/linux-150.0.7871.24/chrome-linux64/chrome',
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu', '--lang=zh-CN'],
});

const results = [];
for (const [name, route] of routes) {
  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 1000, deviceScaleFactor: 1 });
  await page.emulateMediaFeatures([
    { name: 'prefers-color-scheme', value: 'light' },
    { name: 'prefers-reduced-motion', value: 'reduce' },
  ]);
  await page.evaluateOnNewDocument((adminToken) => {
    localStorage.setItem('pool_admin_token', adminToken);
    localStorage.setItem('pool_theme', 'light');
    localStorage.setItem('pool_locale', 'zh');
  }, token);

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
  const result = {
    phase,
    name,
    route,
    file: `${name}.png`,
    failedResponses,
    consoleErrors,
  };
  try {
    await page.goto(`${base}/console${route}`, {
      waitUntil: 'domcontentloaded',
      timeout: 60_000,
    });
    await page.waitForSelector('[data-page-ready="true"]', { timeout: 30_000 });
    await page.evaluate(async () => document.fonts.ready);
    await new Promise((resolve) => setTimeout(resolve, 1_200));
    await page.addStyleTag({
      content: '*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}',
    });
    Object.assign(
      result,
      await page.evaluate(() => {
        const visible = (node) => {
          const rect = node.getBoundingClientRect();
          const style = getComputedStyle(node);
          return (
            rect.width > 0 &&
            rect.height > 0 &&
            style.display !== 'none' &&
            style.visibility !== 'hidden'
          );
        };
        const alerts = [
          ...document.querySelectorAll(
            '[role="alert"],.pool-load-error,.pool-alert,.pool-error-banner',
          ),
        ]
          .filter(visible)
          .map((node) => node.textContent?.trim())
          .filter(Boolean);
        const escapedCellContent = [...document.querySelectorAll('table td')]
          .flatMap((cell, cellIndex) => {
            const cellRect = cell.getBoundingClientRect();
            return [...cell.children]
              .filter(visible)
              .map((child) => ({ child, rect: child.getBoundingClientRect() }))
              .filter(({ rect }) => (
                rect.left < cellRect.left - 1 || rect.right > cellRect.right + 1
              ))
              .map(({ child, rect }) => ({
                cellIndex,
                cellLabel: cell.getAttribute('data-label') || '',
                childText: child.textContent?.trim().slice(0, 120) || '',
                escapedLeft: Math.round((cellRect.left - rect.left) * 10) / 10,
                escapedRight: Math.round((rect.right - cellRect.right) * 10) / 10,
              }));
          });
        return {
          title: document.title,
          finalURL: location.href,
          h1: [...document.querySelectorAll('h1')]
            .map((node) => node.textContent?.trim())
            .filter(Boolean),
          bodyTextLength: document.body.innerText.length,
          horizontalOverflow:
            document.body.scrollWidth > document.body.clientWidth + 1,
          bodyClientWidth: document.body.clientWidth,
          bodyScrollWidth: document.body.scrollWidth,
          visibleCharts: [...document.querySelectorAll('svg.recharts-surface')].filter(
            visible,
          ).length,
          visibleTables: [...document.querySelectorAll('table')].filter(visible).length,
          visibleAlerts: alerts,
          escapedCellContent,
          hasLongAccountFixture: document.body.innerText.includes(
            'very-long-account-label-for-install-upgrade',
          ),
          hasLongEmailFixture: document.body.innerText.includes(
            'apple-layout-long-mailbox-name',
          ),
        };
      }),
    );
    await page.screenshot({
      path: `${outputDir}/${result.file}`,
      fullPage: true,
      type: 'png',
    });
  } catch (error) {
    result.error = error instanceof Error ? error.message : String(error);
    try {
      await page.screenshot({
        path: `${outputDir}/${result.file}`,
        fullPage: true,
        type: 'png',
      });
    } catch {}
  } finally {
    result.loadMillis = Date.now() - startedAt;
    result.ok =
      !result.error &&
      !result.horizontalOverflow &&
      failedResponses.length === 0 &&
      consoleErrors.length === 0 &&
      (result.escapedCellContent?.length || 0) === 0 &&
      (result.visibleAlerts?.length || 0) === 0;
    results.push(result);
    console.log(
      `${result.ok ? 'OK' : 'ISSUE'} ${phase}/${name} ${result.loadMillis}ms`,
    );
    await page.close();
  }
}

await browser.close();
const issues = results.filter((result) => !result.ok);
const report = {
  phase,
  capturedAt: new Date().toISOString(),
  base,
  total: results.length,
  passed: results.length - issues.length,
  issues: issues.length,
  results,
};
fs.writeFileSync(
  `${root}/records/screenshot-${phase}-report.json`,
  `${JSON.stringify(report, null, 2)}\n`,
);
console.log(
  `SCREENSHOT_SUMMARY phase=${phase} total=${report.total} passed=${report.passed} issues=${report.issues}`,
);
if (issues.length) process.exitCode = 1;
