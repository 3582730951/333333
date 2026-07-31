import fs from 'node:fs';
import puppeteer from 'puppeteer-core';
const root = '/root/autodl-tmp/frontend-ui-shot-20260731';
const config = JSON.parse(fs.readFileSync(`${root}/runtime/config.json`, 'utf8'));
const browser = await puppeteer.launch({
  executablePath: '/root/.cache/puppeteer/chrome/linux-150.0.7871.24/chrome-linux64/chrome',
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage', '--disable-gpu'],
});
const page = await browser.newPage();
await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
await page.evaluateOnNewDocument((token) => {
  localStorage.setItem('pool_admin_token', token);
  localStorage.setItem('pool_locale', 'zh');
}, config.admin_token);
await page.goto('http://127.0.0.1:34274/console/settings-v2?tab=registrar', { waitUntil: 'networkidle0' });
await page.waitForSelector('[data-page-ready="true"]');
const result = await page.evaluate(() => {
  const node = document.querySelector('.pool-registrar-actions');
  const form = document.querySelector('.pool-registrar-form');
  const rect = node?.getBoundingClientRect();
  return {
    exists: Boolean(node),
    text: node?.textContent?.trim(),
    html: node?.outerHTML,
    rect: rect ? { x: rect.x, y: rect.y, width: rect.width, height: rect.height } : null,
    display: node ? getComputedStyle(node).display : null,
    visibility: node ? getComputedStyle(node).visibility : null,
    formChildren: form?.children.length,
    formHeight: form?.getBoundingClientRect().height,
  };
});
console.log(JSON.stringify(result, null, 2));
await browser.close();
