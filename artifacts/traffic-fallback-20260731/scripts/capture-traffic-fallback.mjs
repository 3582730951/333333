import fs from 'node:fs';
import puppeteer from 'puppeteer';

const root = process.env.RUN_ROOT || '/root/autodl-tmp/traffic-fallback-20260731';
const base = process.env.BASE_URL || 'http://127.0.0.1:34318';
const token = 'traffic-fallback-review-20260731-token';
const outputDir = `${root}/screenshots`;
fs.mkdirSync(outputDir, { recursive: true });

const sourcePrefix = '流量兜底演示主分组';
const variants = [
  { name: 'desktop-light-editor', theme: 'light', viewport: { width: 1440, height: 1000, deviceScaleFactor: 1 }, selector: false },
  { name: 'desktop-dark-selector', theme: 'dark', viewport: { width: 1440, height: 1000, deviceScaleFactor: 1 }, selector: true, family: 'Claude' },
  { name: 'mobile-light-mapping', theme: 'light', viewport: { width: 390, height: 844, deviceScaleFactor: 1 }, selector: false, mapping: true },
  { name: 'mobile-dark-selector', theme: 'dark', viewport: { width: 390, height: 844, deviceScaleFactor: 1 }, selector: true, family: 'Gemini' },
].filter((variant) => !process.env.VARIANT || variant.name === process.env.VARIANT);

const browser = await puppeteer.launch({
  executablePath: '/root/.cache/puppeteer/chrome/linux-150.0.7871.24/chrome-linux64/chrome',
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu', '--lang=zh-CN'],
});

async function clickByText(page, selector, text) {
  const candidates = await page.$$(selector);
  for (const candidate of candidates) {
    const matches = await candidate.evaluate((node, expected) => (
      node.textContent?.trim() === expected
      && node.getBoundingClientRect().width > 0
      && node.getBoundingClientRect().height > 0
    ), text);
    if (!matches) continue;
    await candidate.click();
    return;
  }
  throw new Error(`missing visible ${selector} text=${text}`);
}

async function openFallbackEditor(page) {
  await clickByText(page, '[role="tab"]', '用户分组');
  await page.waitForFunction(() => [...document.querySelectorAll('[role="tab"]')].some((node) => (
    node.textContent?.trim() === '用户分组' && node.getAttribute('aria-selected') === 'true'
  )), { timeout: 10_000 });
  await page.waitForFunction((prefix) => document.body.innerText.includes(prefix), { timeout: 20_000 }, sourcePrefix);
  const marked = await page.evaluate((prefix) => {
    const textNode = [...document.querySelectorAll('*')].find((node) => (
      node.children.length === 0 && node.textContent?.trim().startsWith(prefix)
    ));
    let scope = textNode?.parentElement || null;
    while (scope && !scope.querySelector('button[aria-label="用户分组操作"]')) scope = scope.parentElement;
    const action = scope?.querySelector('button[aria-label="用户分组操作"]');
    if (!action) return false;
    action.setAttribute('data-screenshot-target', 'source-group-action');
    return true;
  }, sourcePrefix);
  if (!marked) throw new Error('source user-group action menu not found');
  await page.click('[data-screenshot-target="source-group-action"]');
  await page.waitForFunction(() => [...document.querySelectorAll('[role="menuitem"],button')].some((node) => node.textContent?.trim() === '编辑完整策略'), { timeout: 10_000 });
  await clickByText(page, '[role="menuitem"],button', '编辑完整策略');
  await page.waitForSelector('[role="dialog"]', { timeout: 15_000 });
  await page.waitForFunction(() => document.body.innerText.includes('仅在当前分组全部候选失败后接管'), { timeout: 15_000 });
}

const results = [];
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
  }, { token, theme: variant.theme });
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

  const result = { variant: variant.name, theme: variant.theme, failedResponses, consoleErrors };
  try {
    await page.goto(`${base}/console/groups`, { waitUntil: 'domcontentloaded', timeout: 60_000 });
    await page.waitForSelector('[data-page-ready="true"]', { timeout: 30_000 });
    await openFallbackEditor(page);
    await page.evaluate(() => {
      const heading = [...document.querySelectorAll('.pool-card-head, strong')].find((node) => node.textContent?.trim() === '流量兜底');
      heading?.closest('.pool-card')?.scrollIntoView({ block: 'start' });
    });
    await new Promise((resolve) => setTimeout(resolve, 450));

    if (variant.mapping) {
      await page.evaluate(() => document.querySelector('.pool-fallback-mapping-card')?.scrollIntoView({ block: 'center' }));
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    if (variant.selector) {
      await page.evaluate(() => document.querySelector('.pool-fallback-selector__trigger')?.scrollIntoView({ block: 'center' }));
      await new Promise((resolve) => setTimeout(resolve, 150));
      await page.click('.pool-fallback-selector__trigger');
      await page.waitForFunction(() => (
        document.querySelector('.pool-fallback-selector__trigger')?.getAttribute('aria-expanded') === 'true'
        && Boolean(document.querySelector('.pool-fallback-selector__content'))
      ), { timeout: 10_000 }).catch((error) => {
        throw new Error(`fallback selector did not open: ${error.message}`);
      });
      await new Promise((resolve) => setTimeout(resolve, 350));
      if (variant.family) {
        const familyMarked = await page.evaluate((family) => {
          const label = [...document.querySelectorAll('.pool-fallback-selector__families button span')]
            .find((node) => node.textContent?.trim() === family);
          const button = label?.closest('button');
          if (!button) return false;
          button.setAttribute('data-screenshot-family', family);
          return true;
        }, variant.family);
        if (!familyMarked) throw new Error(`missing fallback family ${variant.family}`);
        await page.click(`[data-screenshot-family="${variant.family}"]`);
        await page.waitForFunction((family) => [...document.querySelectorAll('.pool-fallback-selector__families button')]
          .some((node) => node.textContent?.includes(family) && node.getAttribute('aria-pressed') === 'true'), {}, variant.family)
          .catch((error) => {
            throw new Error(`fallback family ${variant.family} did not activate: ${error.message}`);
          });
      }
      await new Promise((resolve) => setTimeout(resolve, 250));
    }
    await page.addStyleTag({ content: '*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}' });
    Object.assign(result, await page.evaluate(() => {
      const dialog = document.querySelector('[role="dialog"]');
      const popover = document.querySelector('.pool-fallback-selector__content');
      const mappingCards = [...document.querySelectorAll('.pool-fallback-mapping-card')];
      const selectedPills = [...document.querySelectorAll('.pool-fallback-selector__pill')].map((node) => node.textContent?.trim());
      const fallbackOptions = [...document.querySelectorAll('.pool-fallback-selector__option-copy strong')].map((node) => node.textContent?.trim());
      const modelValues = [...document.querySelectorAll('.pool-fallback-mapping-card input')].map((node) => node.value).filter(Boolean);
      const boundsOK = (node) => {
        if (!node) return true;
        const rect = node.getBoundingClientRect();
        return rect.left >= -1 && rect.right <= window.innerWidth + 1;
      };
      return {
        actualTheme: document.documentElement.dataset.theme,
        bodyHorizontalOverflow: document.body.scrollWidth > document.body.clientWidth + 1,
        dialogHorizontalOverflow: dialog ? dialog.scrollWidth > dialog.clientWidth + 1 : true,
        popoverVisible: Boolean(popover && popover.getBoundingClientRect().width > 0 && popover.getBoundingClientRect().height > 0),
        popoverInViewport: boundsOK(popover),
        mappingCards: mappingCards.length,
        selectedPills,
        fallbackOptions,
        modelValues,
        hasSourceModel: document.body.innerText.includes('gpt-5.6-sol'),
        hasTargetModel: document.body.innerText.includes('gpt-5.5'),
        hasLongFallbackName: document.body.innerText.includes('GPT 超长兜底用户分组'),
      };
    }));
    result.file = `${variant.name}.png`;
    await page.screenshot({ path: `${outputDir}/${result.file}`, type: 'png', fullPage: false });
    result.ok = failedResponses.length === 0
      && consoleErrors.length === 0
      && result.actualTheme === variant.theme
      && !result.bodyHorizontalOverflow
      && !result.dialogHorizontalOverflow
      && result.popoverInViewport
      && (!variant.selector || (result.popoverVisible && result.fallbackOptions.length > 0))
      && result.mappingCards === 4
      && result.hasSourceModel
      && result.hasTargetModel;
  } catch (error) {
    result.error = error instanceof Error ? error.message : String(error);
    result.ok = false;
    try {
      result.file = `${variant.name}-error.png`;
      await page.screenshot({ path: `${outputDir}/${result.file}`, type: 'png', fullPage: false });
    } catch {}
  } finally {
    results.push(result);
    console.log(`${result.ok ? 'OK' : 'ISSUE'} ${variant.name}`);
    await page.close();
  }
}

await browser.close();
const report = {
  capturedAt: new Date().toISOString(),
  base,
  total: results.length,
  passed: results.filter((result) => result.ok).length,
  issues: results.filter((result) => !result.ok).length,
  results,
};
fs.writeFileSync(`${root}/records/screenshot-report.json`, `${JSON.stringify(report, null, 2)}\n`);
console.log(`SCREENSHOT_SUMMARY total=${report.total} passed=${report.passed} issues=${report.issues}`);
if (report.issues) process.exitCode = 1;
