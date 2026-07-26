'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');

const VALID_PHASES = new Set(['phase1', 'phase1_5', 'phase2', 'phase3']);

function readStdin() {
  return new Promise((resolve, reject) => {
    let raw = '';
    process.stdin.setEncoding('utf8');
    process.stdin.on('data', (chunk) => { raw += chunk; });
    process.stdin.on('end', () => {
      try { resolve(raw.trim() ? JSON.parse(raw) : {}); }
      catch (error) { reject(new Error(`invalid stdin JSON: ${error.message}`)); }
    });
    process.stdin.on('error', reject);
  });
}

function first(...values) {
  for (const value of values) {
    if (value !== undefined && value !== null && String(value).trim() !== '') return value;
  }
  return '';
}

function resolveLegacyDir(config) {
  const candidates = [
    process.env.TURBO_GPT_LEGACY_DIR,
    config.legacy_dir,
    config.legacyDir,
    path.resolve(__dirname, '../../../other_new_gpt_register'),
    path.resolve(process.cwd(), 'other_new_gpt_register'),
  ].filter(Boolean);
  for (const candidate of candidates) {
    const resolved = path.resolve(String(candidate));
    if (fs.existsSync(path.join(resolved, 'index.js'))) return resolved;
  }
  throw new Error(`legacy registrar not found; set TURBO_GPT_LEGACY_DIR (checked ${candidates.join(', ')})`);
}

function legacyConfig(input) {
  const config = { ...(input.config || {}) };
  try { Object.assign(config, JSON.parse(input.job?.config_json || '{}')); } catch (_) {}
  const aliases = {
    heroSmsApiKey: ['hero_sms_api_key', 'HERO_SMS_API_KEY'],
    heroSmsService: ['hero_sms_service', 'HERO_SMS_SERVICE'],
    smsBowerApiKey: ['smsbower_api_key', 'sms_bower_api_key', 'SMSBOWER_API_KEY'],
    smsBowerBaseUrl: ['smsbower_base_url', 'sms_bower_base_url'],
    smsBowerService: ['smsbower_service', 'sms_bower_service'],
    yesCaptchaApiKey: ['yescaptcha_api_key', 'YES_CAPTCHA_API_KEY'],
    mailProvider: ['mail_provider', 'MAIL_PROVIDER'],
    mailBaseUrl: ['mail_base_url', 'MAIL_BASE_URL'],
    mailAdminPassword: ['mail_admin_password', 'MAIL_ADMIN_PASSWORD'],
    mailAdminToken: ['mail_admin_token', 'MAIL_ADMIN_TOKEN'],
    mailDomain: ['mail_domain', 'MAIL_DOMAIN'],
    chromePath: ['chrome_path', 'CHROME_PATH'],
    browserUserDataDir: ['browser_user_data_dir', 'BROWSER_USER_DATA_DIR'],
  };
  for (const [target, sources] of Object.entries(aliases)) {
    if (config[target] !== undefined) continue;
    for (const source of sources) {
      if (config[source] !== undefined) { config[target] = config[source]; break; }
    }
  }
  if (!config.phoneCountryCode && input.job?.phone_country_code) config.phoneCountryCode = input.job.phone_country_code;
  if (!config.mailDomain && input.job?.mail_domain) config.mailDomain = input.job.mail_domain;
  config.heroSmsPromptCountrySelection = false;
  return config;
}

function ensureJobDir(input) {
  const configured = first(input.config?.state_dir, input.config?.stateDir, process.env.TURBO_GPT_STATE_DIR);
  const root = configured ? path.resolve(String(configured)) : path.resolve(process.cwd(), '.run/turbo_gpt_register');
  const id = String(input.job?.id || '').replace(/[^a-zA-Z0-9_.-]/g, '');
  if (!id) throw new Error('job.id required');
  const dir = path.join(root, id);
  fs.mkdirSync(path.join(dir, 'tokens'), { recursive: true });
  return dir;
}

function writeJSON(file, value) {
  fs.writeFileSync(file, JSON.stringify(value, null, 2), { mode: 0o600 });
}

function readJSONArray(file) {
  if (!fs.existsSync(file)) return [];
  const parsed = JSON.parse(fs.readFileSync(file, 'utf8'));
  return Array.isArray(parsed) ? parsed : parsed ? [parsed] : [];
}

function seedState(jobDir, job) {
  if (job.phone) {
    writeJSON(path.join(jobDir, 'accounts.json'), [{
      phone: job.phone,
      password: job.password,
      name: job.full_name,
      birthDate: job.birth_date,
      phoneCountryCode: job.phone_country_code,
      phoneCountryDialCode: job.phone_country_dial_code,
      smsOperator: job.sms_operator,
      status: job.email ? 'email_bound' : 'registered',
      createdAt: new Date().toISOString(),
    }]);
  }
  if (job.email) {
    writeJSON(path.join(jobDir, 'username.json'), [{
      email: job.email,
      phone: job.phone,
      password: job.password,
      name: job.full_name,
      birthDate: job.birth_date,
      phoneCountryCode: job.phone_country_code,
      phoneCountryDialCode: job.phone_country_dial_code,
      smsOperator: job.sms_operator,
      status: 'email_bound',
      createdAt: new Date().toISOString(),
    }]);
  }
}

function runLegacy(legacyDir, jobDir, configPath, args) {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [path.join(legacyDir, 'index.js'), ...args], {
      cwd: jobDir,
      env: {
        ...process.env,
        CONFIG_FILE: configPath,
        REG_PHONE_REUSE_FILE: path.join(jobDir, 'phone-reuse.json'),
      },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let stdout = '';
    let stderr = '';
    const append = (current, chunk) => (current + chunk.toString()).slice(-1024 * 1024);
    child.stdout.on('data', (chunk) => { stdout = append(stdout, chunk); });
    child.stderr.on('data', (chunk) => { stderr = append(stderr, chunk); });
    child.on('error', reject);
    child.on('close', (code, signal) => {
      if (stdout.trim()) process.stderr.write(stdout);
      if (stderr.trim()) process.stderr.write(stderr);
      if (code === 0) resolve();
      else reject(new Error(`legacy registrar exited code=${code} signal=${signal || ''}`));
    });
  });
}

function latestToken(jobDir) {
  const dir = path.join(jobDir, 'tokens');
  if (!fs.existsSync(dir)) return null;
  const files = fs.readdirSync(dir)
    .filter((name) => name.endsWith('.json') && !name.startsWith('old_'))
    .map((name) => ({ name, mtime: fs.statSync(path.join(dir, name)).mtimeMs }))
    .sort((a, b) => b.mtime - a.mtime);
  if (!files.length) return null;
  return JSON.parse(fs.readFileSync(path.join(dir, files[0].name), 'utf8'));
}

function resultFromFiles(jobDir, job) {
  const account = readJSONArray(path.join(jobDir, 'accounts.json')).at(-1) || {};
  const username = readJSONArray(path.join(jobDir, 'username.json')).at(-1) || {};
  return {
    phone: first(username.phone, account.phone, job.phone),
    email: first(username.email, job.email),
    password: first(username.password, account.password, job.password),
    full_name: first(username.name, account.name, job.full_name),
    birth_date: first(username.birthDate, account.birthDate, job.birth_date),
    phone_country_code: first(username.phoneCountryCode, account.phoneCountryCode, job.phone_country_code),
    phone_country_dial_code: first(username.phoneCountryDialCode, account.phoneCountryDialCode, job.phone_country_dial_code),
    sms_platform: first(account.smsPlatform, job.sms_platform),
    sms_operator: first(username.smsOperator, account.smsOperator, job.sms_operator),
    mail_domain: first(job.mail_domain, String(username.email || '').split('@')[1]),
  };
}

async function executePhase(phase, input) {
  const job = input.job || {};
  const config = legacyConfig(input);
  const legacyDir = resolveLegacyDir(config);
  const jobDir = ensureJobDir(input);
  const configPath = path.join(jobDir, 'config.runtime.json');
  writeJSON(configPath, config);
  seedState(jobDir, job);

  if (phase === 'phase1') {
    await runLegacy(legacyDir, jobDir, configPath, ['1', '--stop-after-phase2']);
    return { ...resultFromFiles(jobDir, job), completed_through: 'phase2' };
  }
  if (phase === 'phase1_5') {
    return { ...resultFromFiles(jobDir, job), completed_through: 'phase1_5' };
  }
  if (phase === 'phase2') {
    if (!job.email) await runLegacy(legacyDir, jobDir, configPath, ['--phase2', '--stop-after-phase2']);
    return { ...resultFromFiles(jobDir, job), completed_through: 'phase2' };
  }
  await runLegacy(legacyDir, jobDir, configPath, ['--phase3']);
  const token = latestToken(jobDir);
  if (!token?.refresh_token && !token?.tokens?.refresh_token) throw new Error('phase3 completed without a refresh_token file');
  return { ...resultFromFiles(jobDir, job), token: token.tokens || token };
}

async function main() {
  const phase = String(process.argv[2] || '').trim();
  if (!VALID_PHASES.has(phase)) throw new Error(`unknown phase ${phase || '(empty)'}`);
  const input = await readStdin();
  const data = await executePhase(phase, input);
  process.stdout.write(`${JSON.stringify({ success: true, data })}\n`);
}

main().catch((error) => {
  process.stdout.write(`${JSON.stringify({ success: false, error: error?.message || String(error) })}\n`);
  process.exitCode = 1;
});
