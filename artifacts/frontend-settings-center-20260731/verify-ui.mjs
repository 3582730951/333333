import fs from 'node:fs';
import puppeteer from 'puppeteer-core';

const root = '/root/autodl-tmp/frontend-ui-shot-20260731';
const base = 'http://127.0.0.1:34274';
const consoleBase = `${base}/console`;
const config = JSON.parse(fs.readFileSync(`${root}/runtime/config.json`, 'utf8'));
const outputDir = `${root}/screenshots/apple-v5`;
fs.mkdirSync(outputDir, { recursive: true });

const tabs = ['config', 'automation', 'registrar', 'logging', 'memory', 'thinking', 'moderation'];
const cases = [
  ...tabs.map((tab) => ({ variant: 'desktop-light', theme: 'light', viewport: { width: 1440, height: 1000, deviceScaleFactor: 1 }, tab })),
  ...tabs.map((tab) => ({ variant: 'mobile-light', theme: 'light', viewport: { width: 390, height: 844, deviceScaleFactor: 1 }, tab })),
  { variant: 'desktop-dark', theme: 'dark', viewport: { width: 1440, height: 1000, deviceScaleFactor: 1 }, tab: 'config' },
];

const browser = await puppeteer.launch({
  executablePath: '/root/.cache/puppeteer/chrome/linux-150.0.7871.24/chrome-linux64/chrome',
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu', '--lang=zh-CN'],
});
const results = [];

for (const item of cases) {
  const page = await browser.newPage();
  await page.setViewport(item.viewport);
  await page.emulateMediaFeatures([
    { name: 'prefers-color-scheme', value: item.theme },
    { name: 'prefers-reduced-motion', value: 'reduce' },
  ]);
  await page.evaluateOnNewDocument(({ token, theme }) => {
    localStorage.setItem('pool_admin_token', token);
    localStorage.setItem('pool_theme', theme);
    localStorage.setItem('pool_locale', 'zh');
  }, { token: config.admin_token, theme: item.theme });

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
  await page.goto(`${consoleBase}/settings-v2?tab=${item.tab}`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
  await page.waitForSelector('[data-page-ready="true"]', { timeout: 30_000 });
  await page.evaluate(async () => { await document.fonts.ready; });
  await new Promise((resolve) => setTimeout(resolve, 900));
  await page.addStyleTag({ content: '*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}' });

  const metrics = await page.evaluate((tab) => {
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
    const categories = [...document.querySelectorAll('.pool-settings-category')];
    const alerts = [...document.querySelectorAll('[role="alert"], .pool-load-error, .pool-alert, .pool-error-banner')]
      .filter(visible).map((node) => node.textContent?.trim()).filter(Boolean);
    return {
      h1: h1?.textContent?.trim() || '',
      h1Lines,
      bodyScrollHeight: document.body.scrollHeight,
      bodyScrollWidth: document.body.scrollWidth,
      bodyClientWidth: document.body.clientWidth,
      horizontalOverflow: document.body.scrollWidth > document.body.clientWidth + 1,
      visibleSkeletons: [...document.querySelectorAll('.pool-skel, .pool-skeleton-table, .pool-route-fallback')].filter(visible).length,
      visibleAlerts: alerts,
      tabs: document.querySelectorAll('[role="tab"]').length,
      activeTabs: [...document.querySelectorAll('[role="tab"][aria-selected="true"]')].map((node) => node.textContent?.trim()),
      toolbarButtons: [...document.querySelectorAll('.pool-toolbar button')].filter(visible).length,
      categoryCount: categories.length,
      expandedCategories: categories.filter((node) => node.getAttribute('data-expanded') === 'true').length,
      searchVisible: visible(document.querySelector('input[aria-label="搜索设置"]')),
      tab,
    };
  }, item.tab);

  let interaction = null;
  if (item.tab === 'config') {
    await page.click('input[aria-label="搜索设置"]');
    await page.keyboard.type('goal_retention_days');
    await new Promise((resolve) => setTimeout(resolve, 150));
    interaction = await page.evaluate(() => {
      const visible = (node) => {
        if (!node) return false;
        const rect = node.getBoundingClientRect();
        const style = getComputedStyle(node);
        return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none';
      };
      const categories = [...document.querySelectorAll('.pool-settings-category')];
      return {
        categoryCount: categories.length,
        expandedCategories: categories.filter((node) => node.getAttribute('data-expanded') === 'true').length,
        targetVisible: visible(document.querySelector('[aria-label="目标保留天数"]')),
        technicalKeyVisible: [...document.querySelectorAll('.pool-settings-row__key')]
          .some((node) => visible(node) && node.textContent?.includes('goal_retention_days')),
      };
    });
    await page.screenshot({ path: `${outputDir}/${item.variant}-config-search.png`, fullPage: true, type: 'png' });
    await page.click('button[aria-label="清除"]');
    await new Promise((resolve) => setTimeout(resolve, 100));
    const triggers = await page.$$('.pool-settings-category__trigger');
    if (triggers.length > 1) await triggers[1].click();
    interaction.secondSectionExpanded = await page.evaluate(() => (
      document.querySelectorAll('.pool-settings-category__trigger')[1]?.getAttribute('aria-expanded') === 'true'
    ));
  } else if (item.tab === 'registrar') {
    const firstTrigger = await page.$('.pool-settings-category__trigger');
    if (firstTrigger) await firstTrigger.click();
    await new Promise((resolve) => setTimeout(resolve, 100));
    interaction = await page.evaluate(() => {
      const visible = (node) => {
        if (!node) return false;
        const rect = node.getBoundingClientRect();
        const style = getComputedStyle(node);
        return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none';
      };
      return {
        firstSectionExpanded: document.querySelector('.pool-settings-category__trigger')?.getAttribute('aria-expanded') === 'true',
        providerControlVisible: visible(document.querySelector('input[type="password"]')),
        mountedProviderControls: document.querySelectorAll('input[type="password"]').length,
      };
    });
  }

  const screenshot = `${item.variant}-settings-${item.tab}.png`;
  await page.screenshot({ path: `${outputDir}/${screenshot}`, fullPage: true, type: 'png' });
  const result = {
    variant: item.variant,
    tab: item.tab,
    screenshot,
    loadMillis: Date.now() - startedAt,
    failedResponses,
    consoleErrors,
    interaction,
    ...metrics,
  };
  const commonOK = !result.horizontalOverflow
    && result.h1Lines === 1
    && result.visibleSkeletons === 0
    && result.visibleAlerts.length === 0
    && result.tabs === 7
    && result.activeTabs.length === 1
    && failedResponses.length === 0
    && consoleErrors.length === 0;
  result.ok = commonOK
    && (item.tab !== 'config' || (
      result.categoryCount >= 4
      && result.expandedCategories === 1
      && result.searchVisible
      && result.interaction?.categoryCount === 1
      && result.interaction?.expandedCategories === 1
      && result.interaction?.targetVisible
      && result.interaction?.technicalKeyVisible
      && result.interaction?.secondSectionExpanded
      && result.bodyScrollHeight < (item.variant === 'mobile-light' ? 1_850 : 1_250)
    ))
    && (item.tab !== 'registrar' || (
      result.categoryCount === 5
      && result.expandedCategories === 0
      && result.interaction?.firstSectionExpanded
      && result.interaction?.providerControlVisible
      && result.interaction?.mountedProviderControls >= 6
      && result.bodyScrollHeight < (item.variant === 'mobile-light' ? 1_450 : 1_250)
    ));
  results.push(result);
  console.log(`${result.ok ? 'OK' : 'ISSUE'} ${item.variant}/${item.tab} height=${result.bodyScrollHeight} categories=${result.categoryCount}`);
  await page.close();
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
fs.writeFileSync(`${root}/artifacts/frontend-settings-center/ui-verification.json`, `${JSON.stringify(report, null, 2)}\n`);
console.log(JSON.stringify({ total: report.total, passed: report.passed, issues: report.issues }, null, 2));
if (issues.length) {
  console.log(JSON.stringify(issues, null, 2));
  process.exitCode = 1;
}
