import fs from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const base = process.env.BASE_URL || 'http://127.0.0.1:34277';
const config = JSON.parse(fs.readFileSync(process.env.CONFIG_FILE, 'utf8'));
const output = process.env.OUTPUT_ROOT;
if (!output) throw new Error('OUTPUT_ROOT is required');

fs.rmSync(output, { recursive: true, force: true });
fs.mkdirSync(output, { recursive: true });

const routes = [
  { name: 'dashboard', path: '/', needle: '', chart: true },
  // Long identifiers are deliberately middle-truncated by the responsive
  // Clamp component, so assert their stable visible prefix rather than the
  // hidden source string.
  { name: 'accounts', path: '/accounts', needle: 'Apple 数据可视化' },
  { name: 'email-pool', path: '/email-pool', needle: 'apple-layout-long-mail' },
  { name: 'registration', path: '/registration', needle: '启动注册任务' },
  { name: 'team-lifecycle', path: '/team-lifecycle', needle: 'child-account-with-an-extremely-long-reference' },
  { name: 'cloudflare-mailbox', path: '/email-pool/cloudflare', needle: 'Cloudflare' },
  { name: 'system', path: '/system', needle: '' },
  { name: 'settings-config', path: '/settings-v2?tab=config', needle: '' },
];

const variants = [
  { name: 'desktop-light', theme: 'light', viewport: { width: 1440, height: 1000, deviceScaleFactor: 1 } },
  { name: 'desktop-dark', theme: 'dark', viewport: { width: 1440, height: 1000, deviceScaleFactor: 1 } },
  { name: 'mobile-light', theme: 'light', viewport: { width: 390, height: 844, deviceScaleFactor: 1, isMobile: true, hasTouch: true } },
  { name: 'mobile-dark', theme: 'dark', viewport: { width: 390, height: 844, deviceScaleFactor: 1, isMobile: true, hasTouch: true } },
];

const browser = await puppeteer.launch({
  executablePath: process.env.CHROME_BIN,
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu', '--lang=zh-CN'],
});

const results = [];
try {
  for (const route of routes) {
    for (const variant of variants) {
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

      const consoleErrors = [];
      const failedResponses = [];
      page.on('console', (message) => {
        if (message.type() === 'error') consoleErrors.push(message.text());
      });
      page.on('pageerror', (error) => consoleErrors.push(error.message));
      page.on('response', (response) => {
        if (response.status() >= 400 && response.url().startsWith(base)) {
          failedResponses.push({ status: response.status(), url: response.url() });
        }
      });

      const startedAt = Date.now();
      const file = `${variant.name}-${route.name}.png`;
      const result = {
        route: route.name,
        routePath: route.path,
        variant: variant.name,
        theme: variant.theme,
        viewport: variant.viewport,
        file,
        failedResponses,
        consoleErrors,
      };
      try {
        await page.goto(`${base}/console${route.path}`, {
          waitUntil: 'domcontentloaded',
          timeout: 60_000,
        });
        await page.waitForSelector('[data-page-ready="true"]', { timeout: 45_000 });
        await page.evaluate(async () => document.fonts.ready);
        await new Promise((resolve) => setTimeout(resolve, 900));
        await page.addStyleTag({
          content: '*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}',
        });

        Object.assign(result, await page.evaluate(({ needle }) => {
          const visible = (node) => {
            const rect = node.getBoundingClientRect();
            const style = getComputedStyle(node);
            return rect.width > 0 && rect.height > 0 &&
              style.display !== 'none' && style.visibility !== 'hidden';
          };
          const visibleAlerts = [
            ...document.querySelectorAll('[role="alert"],.pool-load-error,.pool-alert,.pool-error-banner'),
          ].filter(visible).map((node) => node.textContent?.trim()).filter(Boolean);
          const escapedCellContent = [...document.querySelectorAll('table td')]
            .filter(visible)
            .flatMap((cell, cellIndex) => {
              const cellRect = cell.getBoundingClientRect();
              return [...cell.children]
                .filter(visible)
                .map((child) => ({ child, rect: child.getBoundingClientRect() }))
                .filter(({ rect }) => rect.left < cellRect.left - 1 || rect.right > cellRect.right + 1)
                .map(({ child, rect }) => ({
                  cellIndex,
                  label: cell.getAttribute('data-label') || '',
                  text: child.textContent?.trim().slice(0, 120) || '',
                  escapedLeft: Math.round((cellRect.left - rect.left) * 10) / 10,
                  escapedRight: Math.round((rect.right - cellRect.right) * 10) / 10,
                }));
            });
          const longValues = [...document.querySelectorAll('td,dd,.pool-mobile-row,[class*="identity"],[class*="email"]')]
            .filter(visible)
            .map((node) => ({
              text: node.textContent?.trim().replace(/\s+/g, ' ').slice(0, 180) || '',
              width: Math.round(node.getBoundingClientRect().width),
              scrollWidth: node.scrollWidth,
            }))
            .filter((item) => item.text.length >= 54)
            .slice(0, 12);
          const rootStyle = getComputedStyle(document.documentElement);
          return {
            documentTheme: document.documentElement.dataset.theme || '',
            title: document.title,
            finalURL: location.href,
            h1: [...document.querySelectorAll('h1')].filter(visible).map((node) => node.textContent?.trim()).filter(Boolean),
            bodyTextLength: document.body.innerText.length,
            canvasColor: rootStyle.getPropertyValue('--pool-canvas').trim() || getComputedStyle(document.body).backgroundColor,
            horizontalOverflow:
              document.documentElement.scrollWidth > document.documentElement.clientWidth + 1 ||
              document.body.scrollWidth > document.body.clientWidth + 1,
            clientWidth: document.documentElement.clientWidth,
            scrollWidth: Math.max(document.documentElement.scrollWidth, document.body.scrollWidth),
            visibleCharts: [...document.querySelectorAll('svg.recharts-surface')].filter(visible).length,
            visibleTables: [...document.querySelectorAll('table')].filter(visible).length,
            visibleMobileRows: [...document.querySelectorAll('.pool-mobile-row')].filter(visible).length,
            visibleAlerts,
            escapedCellContent,
            longValues,
            fixtureVisible: !needle || document.body.innerText.includes(needle),
            quotaOnePercentVisible:
              !location.pathname.endsWith('/team-lifecycle') ||
              document.body.innerText.includes('1%'),
          };
        }, { needle: route.needle }));

        await page.screenshot({
          path: path.join(output, file),
          fullPage: true,
          type: 'png',
        });

        if (route.name === 'team-lifecycle' && variant.name === 'desktop-dark') {
          const clicked = await page.evaluate(() => {
            const button = [...document.querySelectorAll('button')]
              .find((item) => item.textContent?.trim() === '详情');
            if (!button) return false;
            button.click();
            return true;
          });
          if (clicked) {
            await page.waitForSelector('.pool-lifecycle-events', { timeout: 10_000 });
            await new Promise((resolve) => setTimeout(resolve, 250));
            await page.screenshot({
              path: path.join(output, 'desktop-dark-team-lifecycle-events.png'),
              fullPage: true,
              type: 'png',
            });
            result.detailScreenshot = 'desktop-dark-team-lifecycle-events.png';
          }
        }
      } catch (error) {
        result.error = error instanceof Error ? error.message : String(error);
        try {
          await page.screenshot({ path: path.join(output, file), fullPage: true, type: 'png' });
        } catch {}
      } finally {
        result.loadMillis = Date.now() - startedAt;
        result.ok =
          !result.error &&
          result.documentTheme === variant.theme &&
          !result.horizontalOverflow &&
          (result.visibleAlerts?.length || 0) === 0 &&
          (result.escapedCellContent?.length || 0) === 0 &&
          failedResponses.length === 0 &&
          consoleErrors.length === 0 &&
          result.fixtureVisible === true &&
          result.quotaOnePercentVisible === true &&
          (!route.chart || variant.name.startsWith('mobile-') || result.visibleCharts > 0);
        results.push(result);
        console.log(`${result.ok ? 'OK' : 'ISSUE'} ${variant.name}/${route.name} ${result.loadMillis}ms`);
        await page.close();
      }
    }
  }
} finally {
  await browser.close();
}

const issues = results.filter((result) => !result.ok);
const themes = Object.fromEntries(variants.map(({ name, theme }) => [
  name,
  [...new Set(results.filter((item) => item.variant === name).map((item) => item.canvasColor))],
]));
const report = {
  capturedAt: new Date().toISOString(),
  base,
  total: results.length,
  passed: results.length - issues.length,
  issues: issues.length,
  expectedScreenshots: results.length + (results.some((item) => item.detailScreenshot) ? 1 : 0),
  themes,
  results,
};
fs.writeFileSync(
  path.join(output, 'final-ui-visual-report.json'),
  `${JSON.stringify(report, null, 2)}\n`,
);
console.log(`FINAL_UI_VISUAL total=${report.total} passed=${report.passed} issues=${report.issues} screenshots=${report.expectedScreenshots}`);
if (issues.length) process.exitCode = 1;
