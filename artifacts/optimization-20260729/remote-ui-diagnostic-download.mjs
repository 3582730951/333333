import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import puppeteer from 'puppeteer';

const [baseURL, downloadDir, token, outputPath] = process.argv.slice(2);
if (!baseURL || !downloadDir || !token || !outputPath) {
  throw new Error('usage: node remote-ui-diagnostic-download.mjs BASE_URL DOWNLOAD_DIR TOKEN OUTPUT_JSON');
}

fs.rmSync(downloadDir, { recursive: true, force: true });
fs.mkdirSync(downloadDir, { recursive: true });

const browser = await puppeteer.launch({
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
});

try {
  const page = await browser.newPage();
  const cdp = await page.createCDPSession();
  await cdp.send('Page.setDownloadBehavior', { behavior: 'allow', downloadPath: downloadDir });
  await page.goto(baseURL, { waitUntil: 'domcontentloaded', timeout: 30_000 });
  await page.evaluate((value) => localStorage.setItem('pool_admin_token', value), token);

  const started = Date.now();
  await page.goto(`${baseURL}/audit`, { waitUntil: 'networkidle0', timeout: 30_000 });
  const rendered = await page.evaluate(() => ({
    url: location.href,
    text: (document.body?.innerText || '').slice(0, 2_000),
    buttons: [...document.querySelectorAll('button')].map((button) => (button.textContent || '').trim()),
  }));
  process.stdout.write(`rendered=${JSON.stringify(rendered)}\n`);
  if (/数据读取失败|failed to (load|read) data/i.test(rendered.text)) {
    throw new Error(`audit page rendered an API error: ${rendered.text}`);
  }
  await page.waitForFunction(
    () => [...document.querySelectorAll('button')].some((button) => {
      const text = button.textContent || '';
      return /diagnostic/i.test(text) || text.includes('诊断');
    }),
    { timeout: 30_000 },
  );
  const clickedLabel = await page.evaluate(() => {
    const button = [...document.querySelectorAll('button')].find((candidate) => {
      const text = candidate.textContent || '';
      return /diagnostic/i.test(text) || text.includes('诊断');
    });
    if (!(button instanceof HTMLButtonElement)) throw new Error('diagnostic export button not found');
    const label = (button.textContent || '').trim();
    button.click();
    return label;
  });

  let archivePath = '';
  const deadline = Date.now() + 90_000;
  while (Date.now() < deadline) {
    const names = fs.readdirSync(downloadDir);
    const complete = names.find((name) => name.endsWith('.zip'));
    const partial = names.some((name) => name.endsWith('.crdownload'));
    if (complete && !partial) {
      archivePath = path.join(downloadDir, complete);
      break;
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  if (!archivePath) throw new Error('UI download did not complete within 90 seconds');

  await page.waitForFunction(
    () => [...document.querySelectorAll('button')].some((button) => {
      const text = button.textContent || '';
      return (/diagnostic/i.test(text) || text.includes('诊断')) && !button.disabled;
    }),
    { timeout: 10_000 },
  );

  const bytes = fs.readFileSync(archivePath);
  const result = {
    browser: await browser.version(),
    clicked_label: clickedLabel,
    filename: path.basename(archivePath),
    bytes: bytes.length,
    signature: bytes.subarray(0, 4).toString('hex'),
    elapsed_ms: Date.now() - started,
    button_reenabled: true,
    final_url: page.url(),
  };
  if (result.signature !== '504b0304') throw new Error(`invalid ZIP signature: ${result.signature}`);
  fs.writeFileSync(outputPath, `${JSON.stringify(result, null, 2)}\n`);
  process.stdout.write(`${JSON.stringify(result)}\n`);
} finally {
  await browser.close();
}
