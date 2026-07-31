import fs from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const base = process.env.BASE_URL;
const config = JSON.parse(fs.readFileSync(process.env.CONFIG_FILE, 'utf8'));
const output = process.env.OUTPUT_ROOT;
fs.rmSync(output, { recursive: true, force: true });
fs.mkdirSync(output, { recursive: true });

const variants = [
  ['desktop-light', 'light', { width: 1440, height: 1000, deviceScaleFactor: 1 }],
  ['desktop-dark', 'dark', { width: 1440, height: 1000, deviceScaleFactor: 1 }],
  ['mobile-light', 'light', { width: 390, height: 844, deviceScaleFactor: 1, isMobile: true, hasTouch: true }],
  ['mobile-dark', 'dark', { width: 390, height: 844, deviceScaleFactor: 1, isMobile: true, hasTouch: true }],
];

const browser = await puppeteer.launch({
  executablePath: process.env.CHROME_BIN,
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu', '--lang=zh-CN'],
});
const results = [];
try {
  for (const [name, theme, viewport] of variants) {
    const page = await browser.newPage();
    await page.setViewport(viewport);
    await page.emulateMediaFeatures([
      { name: 'prefers-color-scheme', value: theme },
      { name: 'prefers-reduced-motion', value: 'reduce' },
    ]);
    await page.evaluateOnNewDocument(({ token, selectedTheme }) => {
      localStorage.setItem('pool_admin_token', token);
      localStorage.setItem('pool_theme', selectedTheme);
      localStorage.setItem('pool_locale', 'zh');
    }, { token: config.admin_token, selectedTheme: theme });
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
    await page.goto(`${base}/console/team-lifecycle`, {
      waitUntil: 'domcontentloaded',
      timeout: 60_000,
    });
    await page.waitForSelector('[data-page-ready="true"]', { timeout: 30_000 });
    await page.evaluate(async () => document.fonts.ready);
    await new Promise((resolve) => setTimeout(resolve, 700));
    await page.addStyleTag({
      content: '*,*::before,*::after{animation:none!important;transition:none!important;caret-color:transparent!important}',
    });
    const inspection = await page.evaluate(() => {
      const hero = document.querySelector('.pool-lifecycle-hero');
      const table = document.querySelector('.pool-lifecycle-table');
      return {
        theme: document.documentElement.dataset.theme,
        h1: document.querySelector('h1')?.textContent?.trim() || '',
        hero: Boolean(hero),
        rows: table?.querySelectorAll('tbody tr').length || document.querySelectorAll('.pool-mobile-row').length,
        horizontalOverflow: document.documentElement.scrollWidth > document.documentElement.clientWidth + 1,
        activeText: document.body.innerText.includes('额度监测'),
        thresholdText:
          document.querySelector('.pool-lifecycle-threshold strong')?.textContent?.trim() === '1%',
        longIdentityVisible: document.body.innerText.includes('child-account-with-an-extremely-long-reference'),
      };
    });
    const file = `${name}-team-lifecycle.png`;
    await page.screenshot({ path: path.join(output, file), fullPage: true });
    results.push({
      name,
      file,
      ...inspection,
      failedResponses,
      consoleErrors,
      ok:
        inspection.theme === theme &&
        inspection.h1 === '团队生命周期' &&
        inspection.hero &&
        !inspection.horizontalOverflow &&
        inspection.activeText &&
        inspection.thresholdText &&
        failedResponses.length === 0 &&
        consoleErrors.length === 0,
    });
    if (name === 'desktop-dark') {
      const clicked = await page.evaluate(() => {
        const button = [...document.querySelectorAll('button')].find((item) => item.textContent?.trim() === '详情');
        if (!button) return false;
        button.click();
        return true;
      });
      if (clicked) {
        await page.waitForSelector('.pool-lifecycle-events');
        await new Promise((resolve) => setTimeout(resolve, 300));
        await page.screenshot({
          path: path.join(output, 'desktop-dark-team-lifecycle-events.png'),
          fullPage: true,
        });
      }
    }
    await page.close();
  }
} finally {
  await browser.close();
}
const report = {
  captured_at: new Date().toISOString(),
  total: results.length,
  passed: results.filter((item) => item.ok).length,
  issues: results.filter((item) => !item.ok).length,
  results,
};
fs.writeFileSync(path.join(output, 'team-lifecycle-visual-report.json'), `${JSON.stringify(report, null, 2)}\n`);
console.log(`TEAM_LIFECYCLE_VISUAL total=${report.total} passed=${report.passed} issues=${report.issues}`);
if (report.issues) process.exitCode = 1;
