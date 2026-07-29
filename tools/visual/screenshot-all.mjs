import puppeteer from 'puppeteer';
import { mkdirSync, existsSync } from 'fs';
import { join } from 'path';

const wait = (ms) => new Promise(r => setTimeout(r, ms));

// Create output directory with timestamp
const timestamp = new Date().toISOString().replace(/[:.]/g, '-').slice(0, 19);
const outputDir = join(import.meta.dirname, '..', 'pngs_s', timestamp);
mkdirSync(outputDir, { recursive: true });
console.log(`Screenshot directory: ${outputDir}`);

const BASE_URL = 'http://127.0.0.1:8799/console';
const TOKEN = 'testadmin_token_local';

// All pages to screenshot
const pages = [
  // Auth
  { name: '00-login', path: '/login', wait: 2000 },

  // Admin pages (after setting token)
  { name: '01-dashboard', path: '/', wait: 2000 },
  { name: '02-accounts', path: '/accounts', wait: 2000 },
  { name: '03-groups', path: '/groups', wait: 1500 },
  { name: '04-egress', path: '/egress', wait: 1500 },
  { name: '05-providers', path: '/providers', wait: 1500 },
  { name: '06-registration', path: '/registration', wait: 2000 },
  { name: '07-automation', path: '/automation', wait: 2000 },
  { name: '09-usage', path: '/usage', wait: 2000 },
  { name: '10-quota', path: '/quota', wait: 1500 },
  { name: '11-cf-events', path: '/cf-events', wait: 1500 },
  { name: '12-audit', path: '/audit', wait: 1500 },
  { name: '13-keys', path: '/keys', wait: 1500 },
  { name: '14-users', path: '/users', wait: 1500 },
  { name: '15-thinking', path: '/thinking', wait: 1000 },
  { name: '16-moderation', path: '/moderation', wait: 1000 },
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
    // First, set admin token via login page
    const loginPage = await browser.newPage();
    await loginPage.goto(`${BASE_URL}/login`, { waitUntil: 'networkidle0' });
    await wait(1000);

    // Set token in localStorage
    await loginPage.evaluate((token) => {
      localStorage.setItem('admin_token', token);
    }, TOKEN);

    await loginPage.screenshot({
      path: join(outputDir, '00-login.png'),
      fullPage: false
    });
    console.log('✓ Captured login page');
    await loginPage.close();

    // Now visit each admin page
    for (const item of pages.slice(1)) {
      const page = await browser.newPage();
      await page.setViewport({ width: 1440, height: 900 });

      // Inject token
      await page.goto(`${BASE_URL}${item.path}`, { waitUntil: 'networkidle0', timeout: 30000 });
      await page.evaluate((token) => {
        localStorage.setItem('admin_token', token);
      }, TOKEN);

      // Reload to apply token
      await page.reload({ waitUntil: 'networkidle0' });
      await wait(item.wait);

      const filename = `${item.name}.png`;
      await page.screenshot({
        path: join(outputDir, filename),
        fullPage: true
      });
      console.log(`✓ Captured ${item.name}`);
      await page.close();
    }

    console.log(`\n✅ All screenshots saved to: ${outputDir}`);
    console.log(`Total: ${pages.length} pages`);
  } finally {
    await browser.close();
  }
}

takeScreenshots().catch(console.error);
