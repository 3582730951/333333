import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { chromium } from 'playwright';

const OAUTH_AUTHORIZE_URL = 'https://auth.openai.com/oauth/authorize';
const OAUTH_CLIENT_ID = 'app_EMoamEEZ73f0CkXaXp7hrann';
const OAUTH_REDIRECT_URI = 'http://localhost:1455/auth/callback';
const OAUTH_SCOPE = 'openid profile email offline_access api.connectors.read api.connectors.invoke';
const MAX_FLOW_MILLIS = 5 * 60 * 1000;

function fail(errorClass) {
  process.stderr.write(`[node-registrar] ${errorClass}\n`);
  process.exitCode = 1;
}

function readConfig() {
  const configPath = String(process.env.CONFIG_FILE || '').trim();
  if (!path.isAbsolute(configPath)) {
    throw new Error('config_path_invalid');
  }
  const stat = fs.lstatSync(configPath);
  if (!stat.isFile() || stat.isSymbolicLink() || (stat.mode & 0o077) !== 0) {
    throw new Error('config_permissions_invalid');
  }
  const config = JSON.parse(fs.readFileSync(configPath, 'utf8'));
  for (const key of ['browserUserDataDir', 'resultPath', 'phoneNumber', 'otpRelayURL', 'otpRelayToken']) {
    if (typeof config[key] !== 'string' || config[key].trim() === '') {
      throw new Error('config_missing_required_field');
    }
  }
  if (!path.isAbsolute(config.browserUserDataDir) || !path.isAbsolute(config.resultPath)) {
    throw new Error('config_output_path_invalid');
  }
  const relay = new URL(config.otpRelayURL);
  if (relay.protocol !== 'http:' || !['127.0.0.1', '::1', 'localhost'].includes(relay.hostname)) {
    throw new Error('otp_relay_origin_invalid');
  }
  return config;
}

function base64url(bytes) {
  return Buffer.from(bytes).toString('base64url');
}

function makePKCE() {
  const verifier = base64url(crypto.randomBytes(64));
  const challenge = base64url(crypto.createHash('sha256').update(verifier).digest());
  return { verifier, challenge };
}

function authorizeURL(challenge, state) {
  const query = new URLSearchParams({
    client_id: OAUTH_CLIENT_ID,
    response_type: 'code',
    redirect_uri: OAUTH_REDIRECT_URI,
    scope: OAUTH_SCOPE,
    state,
    code_challenge: challenge,
    code_challenge_method: 'S256',
    id_token_add_organizations: 'true',
    codex_cli_simplified_flow: 'true',
    originator: 'codex_cli_rs'
  });
  return `${OAUTH_AUTHORIZE_URL}?${query.toString()}`;
}

function proxyOptions(config) {
  const host = String(config.proxyHost || '').trim();
  const port = Number(config.proxyPort || 0);
  if (!host || !Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error('registration_proxy_invalid');
  }
  const proxy = { server: `http://${host}:${port}` };
  if (String(config.proxyUsername || '').trim()) {
    proxy.username = String(config.proxyUsername);
    proxy.password = String(config.proxyPassword || '');
  }
  return proxy;
}

function deterministicPresentation(config) {
  const seed = crypto.createHash('sha256').update(String(config.fingerprintSeed || '')).digest();
  const widths = [1280, 1366, 1440];
  const heights = [720, 768, 900];
  const width = Number(config.viewportWidth) || widths[seed[0] % widths.length];
  const height = Number(config.viewportHeight) || heights[seed[1] % heights.length];
  return {
    viewport: { width, height },
    userAgent: String(config.userAgent || '').trim() || undefined,
    locale: String(config.locale || 'en-US'),
    timezoneId: String(config.timezoneId || 'America/New_York')
  };
}

async function visible(locator) {
  try {
    return (await locator.count()) > 0 && await locator.first().isVisible();
  } catch {
    return false;
  }
}

async function clickFirst(page, labels) {
  for (const label of labels) {
    const exact = page.getByRole('button', { name: label, exact: true });
    if (await visible(exact)) {
      await exact.first().click();
      return true;
    }
    const text = page.getByText(label, { exact: true });
    if (await visible(text)) {
      await text.first().click();
      return true;
    }
  }
  return false;
}

async function submit(page) {
  const button = page.locator('button[type="submit"]:visible');
  if (await visible(button)) {
    await button.first().click();
    return;
  }
  await page.keyboard.press('Enter');
}

async function fetchOTP(config) {
  const response = await fetch(config.otpRelayURL, {
    method: 'POST',
    headers: {
      authorization: `Bearer ${config.otpRelayToken}`,
      accept: 'application/json'
    },
    signal: AbortSignal.timeout(190_000)
  });
  if (!response.ok) {
    throw new Error('otp_unavailable');
  }
  const body = await response.json();
  const code = String(body.code || '').trim();
  if (!/^\d{4,10}$/.test(code)) {
    throw new Error('otp_invalid');
  }
  return code;
}

async function fillProfile(page) {
  let changed = false;
  const name = page.locator('input[name="name"]:visible, input[autocomplete="name"]:visible');
  if (await visible(name) && await name.first().inputValue() === '') {
    const first = ['Alex', 'Jordan', 'Taylor', 'Morgan'][crypto.randomInt(4)];
    const last = ['Miller', 'Davis', 'Wilson', 'Anderson'][crypto.randomInt(4)];
    await name.first().fill(`${first} ${last}`);
    changed = true;
  }
  const age = page.locator('input[name="age"]:visible, input[type="number"]:visible');
  if (await visible(age) && await age.first().inputValue() === '') {
    await age.first().fill(String(crypto.randomInt(21, 35)));
    changed = true;
  }
  const checkbox = page.locator('input[type="checkbox"]:visible');
  if (await visible(checkbox) && !(await checkbox.first().isChecked())) {
    await checkbox.first().check();
    changed = true;
  }
  if (changed) {
    await submit(page);
  }
  return changed;
}

function parseCallback(rawURL, expectedState) {
  const callback = new URL(rawURL);
  if (callback.origin !== 'http://localhost:1455' || callback.pathname !== '/auth/callback') {
    throw new Error('oauth_callback_origin_invalid');
  }
  if (callback.searchParams.get('state') !== expectedState) {
    throw new Error('oauth_state_mismatch');
  }
  const code = String(callback.searchParams.get('code') || '').trim();
  if (!code) {
    throw new Error('oauth_code_missing');
  }
  return code;
}

function writeResult(resultPath, result) {
  fs.mkdirSync(path.dirname(resultPath), { recursive: true, mode: 0o700 });
  const partial = `${resultPath}.partial`;
  const fd = fs.openSync(partial, 'wx', 0o600);
  try {
    fs.writeFileSync(fd, `${JSON.stringify(result)}\n`, { encoding: 'utf8' });
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
  fs.renameSync(partial, resultPath);
  const dirFD = fs.openSync(path.dirname(resultPath), fs.constants.O_RDONLY);
  try {
    fs.fsyncSync(dirFD);
  } finally {
    fs.closeSync(dirFD);
  }
}

async function run() {
  const config = readConfig();
  const { verifier, challenge } = makePKCE();
  const state = base64url(crypto.randomBytes(24));
  const presentation = deterministicPresentation(config);
  let callbackURL = '';
  let context;

  try {
    context = await chromium.launchPersistentContext(config.browserUserDataDir, {
      headless: config.headless !== false,
      executablePath: String(config.browserExecutablePath || '').trim() || undefined,
      proxy: proxyOptions(config),
      locale: presentation.locale,
      timezoneId: presentation.timezoneId,
      viewport: presentation.viewport,
      userAgent: presentation.userAgent,
      ignoreDefaultArgs: ['--enable-automation'],
      args: [
        '--no-sandbox',
        '--disable-dev-shm-usage',
        '--disable-blink-features=AutomationControlled'
      ]
    });
    await context.addInitScript(() => {
      Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
    });
    await context.route('http://localhost:1455/auth/callback**', async (route) => {
      callbackURL = route.request().url();
      await route.fulfill({
        status: 200,
        contentType: 'text/plain; charset=utf-8',
        body: 'Authorization complete. You may close this window.'
      });
    });

    const pages = context.pages();
    const page = pages[0] || await context.newPage();
    await page.goto(authorizeURL(challenge, state), {
      waitUntil: 'domcontentloaded',
      timeout: 60_000
    });

    const deadline = Date.now() + MAX_FLOW_MILLIS;
    let phoneSubmitted = false;
    let otpSubmitted = false;
    while (Date.now() < deadline && !callbackURL) {
      const currentURL = page.url();
      if (currentURL.startsWith(OAUTH_REDIRECT_URI) && currentURL.includes('code=')) {
        callbackURL = currentURL;
        break;
      }
      const title = (await page.title().catch(() => '')).toLowerCase();
      if (title.includes('just a moment')) {
        await page.waitForTimeout(2_000);
        continue;
      }

      const phone = page.locator('input[type="tel"]:visible, input[name="phone"]:visible');
      if (await visible(phone) && !phoneSubmitted) {
        const formatted = config.phoneNumber.startsWith('+') ? config.phoneNumber : `+${config.phoneNumber}`;
        await phone.first().fill(formatted);
        await submit(page);
        phoneSubmitted = true;
        await page.waitForTimeout(1_500);
        continue;
      }

      const otp = page.locator(
        'input[name="code"]:visible, input[autocomplete="one-time-code"]:visible, input[inputmode="numeric"]:visible'
      );
      if (await visible(otp) && phoneSubmitted && !otpSubmitted) {
        const code = await fetchOTP(config);
        await otp.first().fill(code);
        await submit(page);
        otpSubmitted = true;
        await page.waitForTimeout(1_500);
        continue;
      }

      if (await fillProfile(page)) {
        await page.waitForTimeout(1_500);
        continue;
      }
      if (await clickFirst(page, ['Sign up', 'Create account', 'Continue with phone'])) {
        await page.waitForTimeout(1_500);
        continue;
      }
      if (await clickFirst(page, ['Authorize', 'Allow', 'Accept', 'Continue', 'Confirm', 'Next'])) {
        await page.waitForTimeout(1_500);
        continue;
      }

      const email = page.locator('input[type="email"]:visible, input[name="email"]:visible');
      const password = page.locator('input[type="password"]:visible');
      if (await visible(email) || await visible(password)) {
        throw new Error('unexpected_email_or_password_challenge');
      }
      await page.waitForTimeout(1_000);
    }
    if (!callbackURL) {
      throw new Error('oauth_callback_timeout');
    }
    const authorizationCode = parseCallback(callbackURL, state);
    writeResult(config.resultPath, {
      type: 'codex_oauth_code',
      authorization_code: authorizationCode,
      code_verifier: verifier,
      redirect_uri: OAUTH_REDIRECT_URI
    });
  } finally {
    if (context) {
      await context.close().catch(() => {});
    }
  }
}

run().catch((error) => {
  const errorClass = /^[a-z0-9_]+$/.test(String(error?.message || ''))
    ? String(error.message)
    : 'internal_failure';
  fail(errorClass);
});
