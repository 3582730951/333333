import puppeteer from 'puppeteer';
import { mkdirSync } from 'fs';
import { join } from 'path';

const wait = (ms) => new Promise(r => setTimeout(r, ms));

// Create output directory with timestamp
const timestamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
const outputDir = join(import.meta.dirname, '..', 'pngs_s', timestamp);
mkdirSync(outputDir, { recursive: true });
console.log(`Screenshot directory: ${outputDir}`);

const BASE_URL = 'http://127.0.0.1:8799/console';
const TOKEN = 'testadmin_token_local';
const TOKEN_KEY = 'pool_admin_token';  // Correct key!

// All pages to screenshot
const adminPages = [
  { name: '01-dashboard', path: '/', wait: 2000 },
  { name: '02-accounts', path: '/accounts', wait: 2000 },
  { name: '03-groups', path: '/groups', wait: 1500 },
  { name: '04-egress', path: '/egress', wait: 1500 },
  { name: '05-providers', path: '/providers', wait: 1500 },
  { name: '06-registration', path: '/registration', wait: 2000 },
  { name: '07-automation', path: '/automation', wait: 2000 },
  { name: '08-lifecycle', path: '/lifecycle', wait: 2000 },
  { name: '09-usage', path: '/usage', wait: 2000 },
  { name: '10-quota', path: '/quota', wait: 1500 },
  { name: '11-cf-events', path: '/cf-events', wait: 1500 },
  { name: '12-audit', path: '/audit', wait: 1500 },
  { name: '13-keys', path: '/keys', wait: 1500 },
  { name: '14-users', path: '/users', wait: 1500 },
  { name: '15-thinking', path: '/thinking', wait: 1000 },
  { name: '16-moderation', path: '/moderation', wait: 1000 },
  { name: '17-gopay', path: '/gopay', wait: 1500 },
  { name: '18-settings', path: '/settings', wait: 2000 },
  { name: '19-system', path: '/system', wait: 2000 },
];

const portalPages = [
  { name: '20-portal-dashboard', path: '/portal', wait: 2000 },
  { name: '21-portal-keys', path: '/portal/keys', wait: 1500 },
  { name: '22-portal-profile', path: '/portal/profile', wait: 1500 },
];

async function takeScreenshots() {
  const browser = await puppeteer.launch({
    headless: 'new',
    executablePath: '/usr/bin/google-chrome-stable',
    args: [
      '--no-sandbox',
      '--disable-setuid-sandbox',
      '--disable-dev-shm-usage',
      '--disable-gpu',
    ],
  });

  try {
    // 1. Capture LOGIN PAGE (without token - should show login form)
    console.log('\n--- Login Page (without auth) ---');
    const loginPage = await browser.newPage();
    await loginPage.setViewport({ width: 1440, height: 900 });

    // Clear any existing localStorage first
    await loginPage.goto(`${BASE_URL}/login`, { waitUntil: 'networkidle0' });
    await loginPage.evaluate(() => {
      localStorage.clear();
    });

    // Reload to get clean login page
    await loginPage.reload({ waitUntil: 'networkidle0' });
    await wait(2000);

    // Light mode login page
    await loginPage.screenshot({
      path: join(outputDir, 'light-00-login.png'),
      fullPage: false
    });
    console.log('✓ Captured light-00-login.png (login form)');

    // Dark mode login page
    await loginPage.evaluate(() => {
      document.documentElement.setAttribute('theme', 'dark');
      localStorage.setItem('semi-mode', 'dark');
    });
    await wait(500);
    await loginPage.screenshot({
      path: join(outputDir, 'dark-00-login.png'),
      fullPage: false
    });
    console.log('✓ Captured dark-00-login.png (login form)');
    await loginPage.close();

    // 2. Now authenticate and capture all admin pages
    // First, authenticate via API call
    console.log('\n--- Authenticating ---');
    const authPage = await browser.newPage();
    await authPage.setViewport({ width: 1440, height: 900 });

    // Set token first, then navigate to trigger me() check
    await authPage.goto(`${BASE_URL}/login`, { waitUntil: 'networkidle0' });
    await authPage.evaluate((token, key) => {
      localStorage.setItem(key, token);
    }, TOKEN, TOKEN_KEY);

    // Navigate to dashboard which will trigger auth check
    await authPage.goto(`${BASE_URL}/`, { waitUntil: 'networkidle0', timeout: 30000 });
    await wait(3000);

    // Check if we're authenticated
    const isAdmin = await authPage.evaluate(() => {
      return document.querySelector('text=Pool 控制台') !== null ||
             document.body.innerText.includes('Pool 控制台') ||
             document.body.innerText.includes('仪表盘');
    });
    console.log('Admin authenticated:', isAdmin);
    await authPage.close();

    // Light mode - all admin pages
    console.log('\n--- Light Mode Admin Pages ---');
    for (const item of adminPages) {
      try {
        const page = await browser.newPage();
        await page.setViewport({ width: 1440, height: 900 });

        // Set token and theme
        await page.evaluate((token, key) => {
          localStorage.setItem(key, token);
          localStorage.removeItem('pool_theme');
          document.documentElement.removeAttribute('theme');
        }, TOKEN, TOKEN_KEY);

        await page.goto(`${BASE_URL}${item.path}`, { waitUntil: 'networkidle0', timeout: 30000 });
        await wait(item.wait);

        await page.screenshot({
          path: join(outputDir, `light-${item.name}.png`),
          fullPage: true
        });
        console.log(`✓ Captured light-${item.name}.png`);
        await page.close();
      } catch (e) {
        console.error(`✗ Error capturing light-${item.name}: ${e.message}`);
      }
    }

    // Dark mode - all admin pages
    console.log('\n--- Dark Mode Admin Pages ---');
    for (const item of adminPages) {
      try {
        const page = await browser.newPage();
        await page.setViewport({ width: 1440, height: 900 });

        // Set token and theme
        await page.evaluate((token, key) => {
          localStorage.setItem(key, token);
          localStorage.setItem('pool_theme', 'dark');
          document.documentElement.setAttribute('theme', 'dark');
        }, TOKEN, TOKEN_KEY);

        await page.goto(`${BASE_URL}${item.path}`, { waitUntil: 'networkidle0', timeout: 30000 });
        await wait(item.wait);

        await page.screenshot({
          path: join(outputDir, `dark-${item.name}.png`),
          fullPage: true
        });
        console.log(`✓ Captured dark-${item.name}.png`);
        await page.close();
      } catch (e) {
        console.error(`✗ Error capturing dark-${item.name}: ${e.message}`);
      }
    }

    // Portal pages - Light mode
    console.log('\n--- Portal Light Mode ---');
    for (const item of portalPages) {
      try {
        const page = await browser.newPage();
        await page.setViewport({ width: 1440, height: 900 });

        await page.evaluate((token, key) => {
          localStorage.setItem(key, token);
          localStorage.removeItem('pool_theme');
        }, TOKEN, TOKEN_KEY);

        await page.goto(`${BASE_URL}${item.path}`, { waitUntil: 'networkidle0', timeout: 30000 });
        await wait(item.wait);
        await page.screenshot({
          path: join(outputDir, `light-${item.name}.png`),
          fullPage: true
        });
        console.log(`✓ Captured light-${item.name}.png`);
        await page.close();
      } catch (e) {
        console.error(`✗ Error capturing light-${item.name}: ${e.message}`);
      }
    }

    // Portal pages - Dark mode
    console.log('\n--- Portal Dark Mode ---');
    for (const item of portalPages) {
      try {
        const page = await browser.newPage();
        await page.setViewport({ width: 1440, height: 900 });

        await page.evaluate((token, key) => {
          localStorage.setItem(key, token);
          localStorage.setItem('pool_theme', 'dark');
        }, TOKEN, TOKEN_KEY);

        await page.goto(`${BASE_URL}${item.path}`, { waitUntil: 'networkidle0', timeout: 30000 });
        await wait(item.wait);
        await page.screenshot({
          path: join(outputDir, `dark-${item.name}.png`),
          fullPage: true
        });
        console.log(`✓ Captured dark-${item.name}.png`);
        await page.close();
      } catch (e) {
        console.error(`✗ Error capturing dark-${item.name}: ${e.message}`);
      }
    }

    // Capture OAuth Login Modal
    console.log('\n--- OAuth Modal ---');
    const modalPage = await browser.newPage();
    await modalPage.setViewport({ width: 1440, height: 900 });

    await modalPage.evaluate((token, key) => {
      localStorage.setItem(key, token);
    }, TOKEN, TOKEN_KEY);

    await modalPage.goto(`${BASE_URL}/accounts`, { waitUntil: 'networkidle0' });
    await wait(3000);

    // Find and click "添加账号" button
    const buttons = await modalPage.$$('button');
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
    await modalPage.screenshot({
      path: join(outputDir, 'light-23-oauth-modal.png'),
      fullPage: true
    });
    console.log('✓ Captured light-23-oauth-modal.png');

    // Dark mode modal
    await modalPage.evaluate(() => {
      document.documentElement.setAttribute('theme', 'dark');
    });
    await wait(500);
    await modalPage.screenshot({
      path: join(outputDir, 'dark-23-oauth-modal.png'),
      fullPage: true
    });
    console.log('✓ Captured dark-23-oauth-modal.png');
    await modalPage.close();

    console.log(`\n✅ All screenshots saved to: ${outputDir}`);

    // Count screenshots
    const { readdirSync } = await import('fs');
    const files = readdirSync(outputDir);
    console.log(`Total screenshots: ${files.length}`);
  } finally {
    await browser.close();
  }
}

takeScreenshots().catch(console.error);