import puppeteer from 'puppeteer';
import { mkdirSync } from 'fs';
import { join } from 'path';

const wait = (ms) => new Promise(r => setTimeout(r, ms));

async function main() {
  const timestamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
  const outputDir = join(import.meta.dirname, '..', 'pngs_s', timestamp);
  mkdirSync(outputDir, { recursive: true });
  console.log('Output:', outputDir);

  const browser = await puppeteer.launch({
    headless: 'new',
    executablePath: '/usr/bin/google-chrome-stable',
    args: ['--no-sandbox', '--disable-setuid-sandbox', '--disable-dev-shm-usage', '--disable-gpu'],
  });

  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 900 });

  // Go to accounts page
  await page.goto('http://127.0.0.1:8799/console/accounts', { waitUntil: 'networkidle0' });

  // Set token
  await page.evaluate(() => {
    localStorage.setItem('admin_token', 'testadmin_token_local');
  });

  // Reload
  await page.reload({ waitUntil: 'networkidle0' });
  await wait(3000);

  // Find and click the "添加账号" button (it's in the header actions)
  const buttons = await page.$$('button');
  for (const btn of buttons) {
    const text = await btn.evaluate(el => el.textContent);
    if (text.includes('添加账号')) {
      console.log('Found button:', text);
      await btn.click();
      await wait(2000);
      break;
    }
  }

  // Light mode modal
  await page.screenshot({ path: join(outputDir, 'light-23-oauth-modal.png'), fullPage: true });
  console.log('✓ Captured light-23-oauth-modal.png');

  // Dark mode modal
  await page.evaluate(() => {
    document.documentElement.setAttribute('theme', 'dark');
  });
  await wait(500);
  await page.screenshot({ path: join(outputDir, 'dark-23-oauth-modal.png'), fullPage: true });
  console.log('✓ Captured dark-23-oauth-modal.png');

  await browser.close();
  console.log('Done!');
}

main().catch(console.error);
