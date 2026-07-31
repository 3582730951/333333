import fs from 'node:fs';
import puppeteer from 'puppeteer-core';

const root = '/root/autodl-tmp/frontend-ui-shot-20260731';
const base = 'http://127.0.0.1:34274';
const config = JSON.parse(fs.readFileSync(`${root}/runtime/config.json`, 'utf8'));
const baselineReport = JSON.parse(fs.readFileSync(`${root}/artifacts/ui-matrix-report.json`, 'utf8'));
const baseline = baselineReport.results.find((item) => item.variant === 'mobile-light' && item.name === '12-model-quality');
const outputDir = `${root}/screenshots/optimized`;
fs.mkdirSync(outputDir, { recursive: true });

const browser = await puppeteer.launch({
  executablePath: '/root/.cache/puppeteer/chrome/linux-150.0.7871.24/chrome-linux64/chrome',
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu', '--lang=zh-CN'],
});
const page = await browser.newPage();
await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
await page.emulateMediaFeatures([
  { name: 'prefers-color-scheme', value: 'light' },
  { name: 'prefers-reduced-motion', value: 'reduce' },
]);
await page.evaluateOnNewDocument((token) => {
  localStorage.setItem('pool_admin_token', token);
  localStorage.setItem('pool_theme', 'light');
  localStorage.setItem('pool_locale', 'zh');
}, config.admin_token);

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
await page.goto(`${base}/console/model-quality`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
await page.waitForSelector('[data-page-ready="true"]', { timeout: 30_000 });
await page.evaluate(async () => { await document.fonts.ready; });
await new Promise((resolve) => setTimeout(resolve, 900));
await page.addStyleTag({ content: '*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}' });

const pageMetrics = await page.evaluate(() => {
  const list = document.querySelector('[role="list"][aria-label="分组模型状态"]');
  const detailButtons = [...document.querySelectorAll('button[aria-label^="查看 "][aria-label$=" 详情"]')];
  return {
    h1: document.querySelector('h1')?.textContent?.trim() || '',
    bodyScrollHeight: document.body.scrollHeight,
    bodyScrollWidth: document.body.scrollWidth,
    bodyClientWidth: document.body.clientWidth,
    horizontalOverflow: document.body.scrollWidth > document.body.clientWidth + 1,
    statusItems: list?.querySelectorAll('[role="listitem"]').length || 0,
    detailButtons: detailButtons.length,
    pager: [...document.querySelectorAll('span')].map((node) => node.textContent?.trim()).find((text) => /^\d+ \/ \d+$/.test(text || '')) || '',
    visibleSkeletons: [...document.querySelectorAll('.pool-skel, .pool-skeleton-table, .pool-route-fallback')]
      .filter((node) => {
        const rect = node.getBoundingClientRect();
        const style = getComputedStyle(node);
        return rect.width > 0 && rect.height > 0 && style.visibility !== 'hidden' && style.display !== 'none';
      }).length,
  };
});
await page.screenshot({ path: `${outputDir}/mobile-light-12-model-quality-after.png`, fullPage: true, type: 'png' });

await page.click('button[aria-label^="查看 "][aria-label$=" 详情"]');
await page.waitForSelector('[role="dialog"]', { visible: true, timeout: 10_000 });
const drawerMetrics = await page.evaluate(() => {
  const dialog = document.querySelector('[role="dialog"]');
  const labels = [...dialog.querySelectorAll('dt')].map((node) => node.textContent?.trim()).filter(Boolean);
  const expectedLabels = ['分组', '模型', '提供方', '智力状态', '最近结果', '连续异常', '答案 / 标准', '返回模型', '累计 Token', '延迟', '最近检测'];
  return {
    title: dialog.querySelector('.pool-drawer-title')?.textContent?.trim() || '',
    labels,
    missingLabels: expectedLabels.filter((label) => !labels.includes(label)),
    hasRunAction: [...dialog.querySelectorAll('button')].some((button) => button.textContent?.trim() === '立即检测' && !button.disabled),
    textLength: dialog.textContent?.trim().length || 0,
  };
});
await page.screenshot({ path: `${outputDir}/mobile-light-12-model-quality-detail-drawer.png`, type: 'png' });

const baselineHeight = Number(baseline?.bodyScrollHeight || 0);
const heightReduction = baselineHeight > 0
  ? Number(((baselineHeight - pageMetrics.bodyScrollHeight) / baselineHeight).toFixed(4))
  : null;
const result = {
  capturedAt: new Date().toISOString(),
  url: page.url(),
  loadMillis: Date.now() - startedAt,
  baseline: {
    screenshot: baseline?.file || '',
    bodyScrollHeight: baselineHeight,
  },
  optimized: pageMetrics,
  heightReduction,
  drawer: drawerMetrics,
  failedResponses,
  consoleErrors,
  screenshots: [
    `${outputDir}/mobile-light-12-model-quality-after.png`,
    `${outputDir}/mobile-light-12-model-quality-detail-drawer.png`,
  ],
};
result.ok = Boolean(
  baselineHeight > 0
  && heightReduction >= 0.5
  && pageMetrics.statusItems === 20
  && pageMetrics.detailButtons === 20
  && pageMetrics.pager === '1 / 2'
  && !pageMetrics.horizontalOverflow
  && pageMetrics.visibleSkeletons === 0
  && drawerMetrics.missingLabels.length === 0
  && drawerMetrics.hasRunAction
  && failedResponses.length === 0
  && consoleErrors.length === 0
);
fs.writeFileSync(`${root}/artifacts/model-quality-mobile-verification.json`, `${JSON.stringify(result, null, 2)}\n`);
console.log(JSON.stringify(result, null, 2));

await browser.close();
if (!result.ok) process.exitCode = 1;
