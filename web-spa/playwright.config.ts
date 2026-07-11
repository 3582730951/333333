import { defineConfig } from '@playwright/test';
import puppeteer from 'puppeteer';

const executablePath = await puppeteer.executablePath();

export default defineConfig({
  testDir: './e2e',
  timeout: 90_000,
  expect: { timeout: 10_000 },
  fullyParallel: false,
  workers: 1,
  reporter: [['list'], ['html', { open: 'never', outputFolder: '.run/playwright-report' }]],
  outputDir: '.run/playwright-artifacts',
  use: {
    baseURL: 'http://127.0.0.1:5188',
    locale: 'zh-CN',
    timezoneId: 'Asia/Shanghai',
    colorScheme: 'light',
    launchOptions: { executablePath },
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  webServer: {
    command: 'npm run dev -- --host 127.0.0.1 --port 5188 --strictPort',
    url: 'http://127.0.0.1:5188/console/',
    reuseExistingServer: true,
    timeout: 60_000,
  },
});
