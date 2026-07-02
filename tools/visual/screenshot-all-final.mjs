import puppeteer from 'puppeteer';
import { mkdirSync, existsSync } from 'fs';
import { join } from 'path';

const wait = (ms) => new Promise(r => setTimeout(r, ms));

// Create output directory with timestamp
const timestamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
const outputDir = join('/mnt/d/Code/R3_Code/MicliProxy/pngs', timestamp);
mkdirSync(outputDir, { recursive: true });
console.log(`Screenshot directory: ${outputDir}`);

// Config - match pool_server config.local9876.json
const BASE_URL = 'http://127.0.0.1:9876/console';
const TOKEN = 'test123';  // admin_token from config.local9876.json

// All pages to screenshot
const pages = [
  // Login page
  { name: '00-login', path: '/login', wait: 2000 },

  // Admin pages (dashboard = root)
  { name: '01-dashboard', path: '/', wait: 2500 },
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

  // User portal pages
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
    // First, capture login page (no auth)
    console.log('\n--- Login Page ---');
    const loginPage = await browser.newPage();
    await loginPage.setViewport({ width: 1440, height: 900 });

    // Clear localStorage first
    await loginPage.goto(`${BASE_URL}/login`, { waitUntil: 'networkidle0' });
    await loginPage.evaluate(() => localStorage.clear());
    await loginPage.reload({ waitUntil: 'networkidle0' });
    await wait(2000);

    // Light mode login
    await loginPage.screenshot({
      path: join(outputDir, 'light-00-login.png'),
      fullPage: false
    });
    console.log('✓ Captured light-00-login.png');

    // Dark mode login
    await loginPage.evaluate(() => {
      document.documentElement.setAttribute('theme', 'dark');
      localStorage.setItem('semi-mode', 'dark');
    });
    await wait(500);
    await loginPage.screenshot({
      path: join(outputDir, 'dark-00-login.png'),
      fullPage: false
    });
    console.log('✓ Captured dark-00-login.png');
    await loginPage.close();

    // Now capture all pages with both light and dark themes
    console.log('\n--- Capturing all pages ---');
    for (const item of pages.slice(1)) { // Skip login, already captured
      // Light mode
      try {
        const pageLight = await browser.newPage();
        await pageLight.setViewport({ width: 1440, height: 900 });

        // Set token
        await pageLight.goto(`${BASE_URL}${item.path}`, { waitUntil: 'networkidle0', timeout: 30000 });
        await pageLight.evaluate((token) => {
          localStorage.setItem('pool_admin_token', token);
        }, TOKEN);

        // Clear theme for light mode
        await pageLight.evaluate(() => {
          localStorage.removeItem('pool_theme');
          document.documentElement.removeAttribute('theme');
        });

        await pageLight.reload({ waitUntil: 'networkidle0' });
        await wait(item.wait);

        await pageLight.screenshot({
          path: join(outputDir, `light-${item.name}.png`),
          fullPage: true
        });
        console.log(`✓ Captured light-${item.name}.png`);
        await pageLight.close();
      } catch (e) {
        console.error(`✗ Error light-${item.name}: ${e.message}`);
      }

      // Dark mode
      try {
        const pageDark = await browser.newPage();
        await pageDark.setViewport({ width: 1440, height: 900 });

        await pageDark.goto(`${BASE_URL}${item.path}`, { waitUntil: 'networkidle0', timeout: 30000 });
        await pageDark.evaluate((token) => {
          localStorage.setItem('pool_admin_token', token);
          localStorage.setItem('pool_theme', 'dark');
          document.documentElement.setAttribute('theme', 'dark');
        }, TOKEN);

        await pageDark.reload({ waitUntil: 'networkidle0' });
        await wait(item.wait);

        await pageDark.screenshot({
          path: join(outputDir, `dark-${item.name}.png`),
          fullPage: true
        });
        console.log(`✓ Captured dark-${item.name}.png`);
        await pageDark.close();
      } catch (e) {
        console.error(`✗ Error dark-${item.name}: ${e.message}`);
      }
    }

    // Try to capture OAuth modal
    console.log('\n--- OAuth Modal ---');
    try {
      const modalPage = await browser.newPage();
      await modalPage.setViewport({ width: 1440, height: 900 });

      await modalPage.goto(`${BASE_URL}/accounts`, { waitUntil: 'networkidle0', timeout: 30000 });
      await modalPage.evaluate((token) => {
        localStorage.setItem('pool_admin_token', token);
      }, TOKEN);
      await modalPage.reload({ waitUntil: 'networkidle0' });
      await wait(2000);

      // Find and click "添加账号" or "导入" button
      const buttons = await modalPage.$$('button');
      for (const btn of buttons) {
        const text = await btn.evaluate(el => el.textContent.trim());
        if (text.includes('添加账号') || text.includes('导入') || text.includes('Add')) {
          console.log('Found button:', text);
          await btn.click();
          await wait(2000);
          break;
        }
      }

      // Light modal
      await modalPage.screenshot({
        path: join(outputDir, 'light-23-oauth-modal.png'),
        fullPage: true
      });
      console.log('✓ Captured light-23-oauth-modal.png');

      // Dark modal
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
    } catch (e) {
      console.error(`✗ OAuth Modal error: ${e.message}`);
    }

    console.log(`\n✅ All screenshots saved to: ${outputDir}`);

    // Count screenshots
    const { readdirSync } = await import('fs');
    const files = readdirSync(outputDir);
    console.log(`Total screenshots: ${files.length}`);
    console.log('Files:', files.join(', '));
  } finally {
    await browser.close();
  }
}

takeScreenshots().catch(console.error);
