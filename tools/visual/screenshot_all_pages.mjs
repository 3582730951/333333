import puppeteer from 'puppeteer-core';

const SCREENSHOT_DIR = process.argv[2] || '/tmp/pngs_s/20260628_230116';
const BASE_URL = 'http://127.0.0.1:9876';
const ADMIN_TOKEN = 'test123';

// Admin pages to screenshot
const adminPages = [
  // Dashboard & Overview
  { name: '01_dashboard', path: '/console/' },

  // Accounts
  { name: '02_accounts', path: '/console/accounts' },

  // Groups
  { name: '03_groups', path: '/console/groups' },

  // Egress
  { name: '04_egress', path: '/console/egress' },

  // Providers
  { name: '05_providers', path: '/console/providers' },

  // Registration
  { name: '06_registration', path: '/console/registration' },
  { name: '07_automation', path: '/console/automation' },

  // Monitoring
  { name: '09_usage', path: '/console/usage' },
  { name: '10_quota', path: '/console/quota' },
  { name: '11_cf_events', path: '/console/cf-events' },
  { name: '12_audit', path: '/console/audit' },

  // System
  { name: '13_keys', path: '/console/keys' },
  { name: '14_users', path: '/console/users' },
  { name: '15_thinking', path: '/console/thinking' },
  { name: '16_moderation', path: '/console/moderation' },
  { name: '18_settings', path: '/console/settings' },

  // User Portal
  { name: '19_portal_dashboard', path: '/console/portal' },
  { name: '20_portal_keys', path: '/console/portal/keys' },
  { name: '21_portal_profile', path: '/console/portal/profile' },

  // Login
  { name: '22_login', path: '/console/login' },
];

const sleep = (ms) => new Promise(resolve => setTimeout(resolve, ms));

async function takeScreenshots() {
  console.log(`Starting screenshot capture to ${SCREENSHOT_DIR}`);
  console.log(`Base URL: ${BASE_URL}\n`);

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

  // Login as admin first
  console.log('Logging in as admin...');
  await page.goto(`${BASE_URL}/console/login`, { waitUntil: 'networkidle0' });
  await sleep(1000);

  // Fill in the token and login
  await page.evaluate((token) => {
    localStorage.setItem('pool_admin_token', token);
  }, ADMIN_TOKEN);

  // Reload to apply the token
  await page.reload({ waitUntil: 'networkidle0' });
  await sleep(1000);

  // Take desktop screenshots (1920x1080)
  console.log('\n=== Desktop screenshots (1920x1080) ===\n');

  for (const p of adminPages) {
    if (p.name === '22_login') {
      // Skip login after we've logged in
      continue;
    }
    try {
      console.log(`Screenshot: ${p.name}`);
      await page.goto(`${BASE_URL}${p.path}`, { waitUntil: 'networkidle2', timeout: 30000 });
      await sleep(2000);

      await page.screenshot({
        path: `${SCREENSHOT_DIR}/${p.name}.png`,
        fullPage: false,
      });
      console.log(`  ✓ ${p.name}.png`);
    } catch (err) {
      console.error(`  ✗ ${p.name}: ${err.message}`);
    }
  }

  // Take mobile screenshots (430x932 - iPhone 14 Pro)
  console.log('\n=== Mobile screenshots (430x932) ===\n');
  await page.setViewport({ width: 430, height: 932 });

  const mobilePages = ['dashboard', 'accounts', 'usage', 'settings', 'groups', 'audit'];
  for (const name of mobilePages) {
    const p = adminPages.find(x => x.name.includes(name));
    if (p) {
      try {
        console.log(`Mobile: ${p.name}`);
        await page.goto(`${BASE_URL}${p.path}`, { waitUntil: 'networkidle2', timeout: 30000 });
        await sleep(2000);
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
  console.log('\n=== Dark mode screenshots (1920x1080) ===\n');
  await page.setViewport({ width: 1920, height: 1080 });

  // Enable dark mode
  await page.evaluate(() => {
    document.documentElement.setAttribute('data-theme', 'dark');
    localStorage.setItem('theme', 'dark');
  });

  const darkPages = ['dashboard', 'accounts', 'usage', 'settings', 'groups', 'audit'];
  for (const name of darkPages) {
    const p = adminPages.find(x => x.name.includes(name));
    if (p) {
      try {
        console.log(`Dark mode: ${p.name}`);
        await page.goto(`${BASE_URL}${p.path}`, { waitUntil: 'networkidle2', timeout: 30000 });
        await sleep(2000);
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
  console.log(`Output directory: ${SCREENSHOT_DIR}`);
}

takeScreenshots().catch(console.error);
