const puppeteer = require('puppeteer-core');

const SCREENSHOT_DIR = process.argv[2] || '/tmp/pngs';
const BASE_URL = 'http://127.0.0.1:9999';
const ADMIN_TOKEN = 'test123';

// Admin pages to screenshot
const adminPages = [
  // Dashboard & Overview
  { name: '01_dashboard', path: '/console/' },
  { name: '02_dashboard_cache', path: '/console/' },

  // Accounts
  { name: '03_accounts', path: '/console/accounts' },
  { name: '04_accounts_detail', path: '/console/accounts', click: '.pool-table tr:first-child td:first-child' },

  // Groups
  { name: '05_groups', path: '/console/groups' },

  // Egress
  { name: '06_egress', path: '/console/egress' },

  // Providers
  { name: '07_providers', path: '/console/providers' },

  // Registration
  { name: '08_registration', path: '/console/registration' },
  { name: '09_automation', path: '/console/automation' },

  // Monitoring
  { name: '11_usage', path: '/console/usage' },
  { name: '12_quota', path: '/console/quota' },
  { name: '13_cf_events', path: '/console/cf-events' },
  { name: '14_audit', path: '/console/audit' },

  // System
  { name: '15_keys', path: '/console/keys' },
  { name: '16_users', path: '/console/users' },
  { name: '17_thinking', path: '/console/thinking' },
  { name: '18_moderation', path: '/console/moderation' },
  { name: '20_settings', path: '/console/settings-v2' },

  // User Portal
  { name: '21_portal_dashboard', path: '/console/portal' },
  { name: '22_portal_keys', path: '/console/portal/keys' },
  { name: '23_portal_profile', path: '/console/portal/profile' },
];

async function takeScreenshots() {
  const browser = await puppeteer.launch({
    headless: true,
    executablePath: process.env.CHROME_PATH || '/usr/bin/google-chrome-stable',
    args: [
      '--no-sandbox',
      '--disable-setuid-sandbox',
      '--disable-dev-shm-usage',
      '--disable-gpu',
    ],
    defaultViewport: { width: 1920, height: 1080 },
  });

  const page = await browser.newPage();
  await page.setViewport({ width: 1920, height: 1080 });

  // Login as admin
  console.log('Logging in as admin...');
  await page.goto(`${BASE_URL}/console/login`, { waitUntil: 'networkidle0' });
  await page.evaluate((token) => {
    localStorage.setItem('admin_token', token);
  }, ADMIN_TOKEN);

  for (const p of adminPages) {
    try {
      console.log(`Screenshot: ${p.name}`);
      await page.goto(`${BASE_URL}${p.path}`, { waitUntil: 'networkidle2', timeout: 30000 });

      // Wait for content to load
      await page.waitForTimeout(2000);

      // Click element if specified
      if (p.click) {
        await page.click(p.click).catch(() => {});
        await page.waitForTimeout(1500);
      }

      await page.screenshot({
        path: `${SCREENSHOT_DIR}/${p.name}.png`,
        fullPage: false,
      });
      console.log(`  ✓ ${p.name}.png`);
    } catch (err) {
      console.error(`  ✗ ${p.name}: ${err.message}`);
    }
  }

  // Take responsive screenshots for mobile view
  console.log('\nTaking mobile view screenshots...');
  await page.setViewport({ width: 430, height: 932 });

  const mobilePages = ['dashboard', 'accounts', 'usage'];
  for (const name of mobilePages) {
    const p = adminPages.find(x => x.name.includes(name));
    if (p) {
      try {
        await page.goto(`${BASE_URL}${p.path}`, { waitUntil: 'networkidle2', timeout: 30000 });
        await page.waitForTimeout(2000);
        await page.screenshot({
          path: `${SCREENSHOT_DIR}/mobile_${p.name}.png`,
          fullPage: false,
        });
        console.log(`  ✓ mobile_${p.name}.png`);
      } catch (err) {
        console.error(`  ✗ mobile_${name}: ${err.message}`);
      }
    }
  }

  // Take dark mode screenshots
  console.log('\nTaking dark mode screenshots...');
  await page.setViewport({ width: 1920, height: 1080 });
  await page.goto(`${BASE_URL}/console/`, { waitUntil: 'networkidle2' });
  await page.evaluate(() => {
    document.documentElement.setAttribute('data-theme', 'dark');
  });

  const darkPages = ['dashboard', 'accounts', 'usage', 'settings'];
  for (const name of darkPages) {
    const p = adminPages.find(x => x.name.includes(name));
    if (p) {
      try {
        await page.goto(`${BASE_URL}${p.path}`, { waitUntil: 'networkidle2', timeout: 30000 });
        await page.waitForTimeout(2000);
        await page.screenshot({
          path: `${SCREENSHOT_DIR}/dark_${p.name}.png`,
          fullPage: false,
        });
        console.log(`  ✓ dark_${p.name}.png`);
      } catch (err) {
        console.error(`  ✗ dark_${name}: ${err.message}`);
      }
    }
  }

  await browser.close();
  console.log('\n✅ All screenshots completed!');
}

takeScreenshots().catch(console.error);
