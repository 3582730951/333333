'use strict';

const fs = require('fs');
const path = require('path');
const pino = require('pino');
const grpc = require('@grpc/grpc-js');
const protoLoader = require('@grpc/proto-loader');
const {
  default: makeWASocket,
  useMultiFileAuthState,
  fetchLatestBaileysVersion,
  Browsers,
} = require('@whiskeysockets/baileys');
const { HttpsProxyAgent } = require('https-proxy-agent');
const { SocksProxyAgent } = require('socks-proxy-agent');

// --- Environment Variables ---
const SESSION_DIR = process.env.WA_SESSION_DIR || path.resolve(__dirname, '.baileys-session');
const STATE_FILE = path.join(SESSION_DIR, 'wa_state.json');
const LOGIN_MODE = 'pairing';
const PAIRING_PHONE = (process.env.WA_PAIRING_PHONE || '').replace(/[^\d]/g, '');
const PROXY_URL = process.env.WA_PROXY_URL || process.env.HTTPS_PROXY || process.env.HTTP_PROXY || '';
const GRPC_PORT = Number(process.env.WA_GRPC_PORT || 50056);
const OTP_PROTO = process.env.WA_OTP_PROTO || path.resolve(__dirname, 'proto/otp.proto');

// --- Filters for GoPay OTP ---
const SENDER_PATTERNS = [/gojek/i, /gopay/i, /midtrans/i];
const GENERIC_OTP_PATTERNS = [
  /\bverification code\b/i,
  /\bverifikasi\b/i,
  /\bone[- ]?time\b/i,
  /\bOTP\b/,
  /\bkode\b/i,
];
if (process.env.WA_OTP_SENDER_REGEX) {
  try {
    SENDER_PATTERNS.push(new RegExp(process.env.WA_OTP_SENDER_REGEX, 'i'));
  } catch (_) { /* ignore bad regex */ }
}
const OTP_REGEX = /\b(\d{6})\b/;

// --- Helpers ---
function writeState(obj) {
  const payload = { ...obj, ts: Date.now() };
  try {
    fs.mkdirSync(path.dirname(STATE_FILE), { recursive: true });
    fs.writeFileSync(STATE_FILE, JSON.stringify(payload, null, 2));
  } catch (e) {
    console.error('[wa] state write failed:', e.message);
  }
}

function log(msg) {
  console.log(`[wa] ${msg}`);
}

function redactJid(value) {
  return String(value || '').replace(/\d(?=\d{2})/g, '*');
}

function redactPhone(value) {
  const digits = String(value || '').replace(/[^\d]/g, '');
  if (!digits) return '';
  return digits.replace(/\d(?=\d{2})/g, '*');
}

function redactProxyUrl(value) {
  return String(value || '').replace(/\/\/.*@/, '//***@');
}

function extractText(message) {
  if (!message) return '';
  if (message.ephemeralMessage) return extractText(message.ephemeralMessage.message);
  if (message.viewOnceMessage) return extractText(message.viewOnceMessage.message);
  if (message.viewOnceMessageV2) return extractText(message.viewOnceMessageV2.message);
  if (message.viewOnceMessageV2Extension) return extractText(message.viewOnceMessageV2Extension.message);
  if (message.documentWithCaptionMessage) return extractText(message.documentWithCaptionMessage.message);
  if (message.editedMessage) return extractText(message.editedMessage.message);
  if (message.conversation) return message.conversation;
  if (message.extendedTextMessage) return message.extendedTextMessage.text || '';
  if (message.imageMessage) return message.imageMessage.caption || '';
  if (message.videoMessage) return message.videoMessage.caption || '';
  if (message.documentMessage) return message.documentMessage.caption || '';
  if (message.buttonsResponseMessage) return message.buttonsResponseMessage.selectedDisplayText || '';
  if (message.templateButtonReplyMessage) return message.templateButtonReplyMessage.selectedDisplayText || '';
  return '';
}

function truncate(value, maxLen = 800) {
  const s = String(value || '');
  return s.length > maxLen ? s.slice(0, maxLen) + '…' : s;
}

function extractSender(msgInfo) {
  const pushName = msgInfo.pushName || '';
  const remoteJid = (msgInfo.key && msgInfo.key.remoteJid) || '';
  return { pushName, remoteJid };
}

// Pre-flight cleanup
try { fs.unlinkSync(STATE_FILE); } catch (_) {}

writeState({ status: 'starting', login_mode: LOGIN_MODE });

let reconnectAttempts = 0;
const otpWaiters = new Set();
let cachedOtp = null;

function finishWaiter(waiter, response) {
  if (waiter.done) return;
  waiter.done = true;
  clearTimeout(waiter.timer);
  otpWaiters.delete(waiter);
  waiter.callback(null, response);
}

function cancelWaiter(waiter, reason) {
  if (waiter.done) return;
  waiter.done = true;
  clearTimeout(waiter.timer);
  otpWaiters.delete(waiter);
  log(`OtpService.WaitForOtp cancelled reason=${reason} waiters=${otpWaiters.size}`);
}

function waitForOtp(call, callback) {
  const req = call.request || {};
  const timeoutSeconds = Math.max(1, Number(req.timeout_seconds || 150));
  const issuedAfterUnix = Number(req.issued_after_unix || Math.floor(Date.now() / 1000));

  if (cachedOtp && cachedOtp.tsUnix >= issuedAfterUnix) {
    const response = {
      found: true,
      otp: cachedOtp.otp,
      source: cachedOtp.source,
      error_message: '',
    };
    cachedOtp = null;
    callback(null, response);
    log('OtpService.WaitForOtp served from cache and cleared');
    return;
  }

  const waiter = {
    purpose: String(req.purpose || 'gopay'),
    issuedAfterUnix,
    callback,
    done: false,
    timer: null,
  };

  waiter.timer = setTimeout(() => {
    finishWaiter(waiter, {
      found: false,
      otp: '',
      source: '',
      error_message: `timeout waiting for OTP after ${timeoutSeconds}s`,
    });
  }, timeoutSeconds * 1000);

  call.on('cancelled', () => cancelWaiter(waiter, 'client_cancelled'));
  call.on('error', (err) => cancelWaiter(waiter, err && err.message ? err.message : 'call_error'));

  otpWaiters.add(waiter);
  log(`OtpService.WaitForOtp purpose=${waiter.purpose} timeout=${timeoutSeconds}s waiters=${otpWaiters.size}`);
}

function deliverOtp(otp, source, remoteJid, messageTsMs) {
  const tsUnix = Math.floor((messageTsMs || Date.now()) / 1000);
  const payload = {
    otp,
    source: `${source}:${redactJid(remoteJid).slice(0, 30)}`,
    tsUnix,
  };

  cachedOtp = payload;

  for (const waiter of Array.from(otpWaiters)) {
    if (waiter.done || tsUnix < waiter.issuedAfterUnix) continue;
    finishWaiter(waiter, {
      found: true,
      otp: payload.otp,
      source: payload.source,
      error_message: '',
    });
    cachedOtp = null;
    log(`OTP captured and delivered (${source})`);
    return;
  }

  log(`OTP captured and cached (${source})`);
}

function startGrpcServer() {
  const packageDefinition = protoLoader.loadSync(OTP_PROTO, {
    keepCase: true,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
  });
  const loaded = grpc.loadPackageDefinition(packageDefinition);
  const server = new grpc.Server();
  server.addService(loaded.otp.OtpService.service, { WaitForOtp: waitForOtp });
  server.bindAsync(`0.0.0.0:${GRPC_PORT}`, grpc.ServerCredentials.createInsecure(), (err, port) => {
    if (err) {
      writeState({ status: 'error', error: 'grpc bind failed: ' + err.message });
      log('grpc bind failed: ' + err.message);
      process.exit(1);
    }
    server.start();
    log(`OtpService gRPC listening on :${port}`);
  });
  return server;
}

async function boot() {
  fs.mkdirSync(SESSION_DIR, { recursive: true });
  const { state, saveCreds } = await useMultiFileAuthState(SESSION_DIR);
  const { version } = await fetchLatestBaileysVersion();
  log(`Baileys WA version: ${version.join('.')}`);

  const sockConfig = {
    version,
    auth: state,
    // Disguise as standard Chrome for better pairing code stability
    browser: Browsers.macOS('Chrome'),
    logger: pino({ level: 'silent' }),
    markOnlineOnConnect: true,
    syncFullHistory: true,
    connectTimeoutMs: 60000,
    defaultQueryTimeoutMs: 60000,
    keepAliveIntervalMs: 10000, // Frequent ping to prevent proxy from dropping idle connection
    retryRequestDelayMs: 2000,
  };

  if (PROXY_URL) {
    if (PROXY_URL.startsWith('socks')) {
      const agent = new SocksProxyAgent(PROXY_URL);
      sockConfig.agent = agent;
      sockConfig.fetchAgent = agent;
      sockConfig.options = { agent }; // Ensure ws receives the agent
    } else {
      const agent = new HttpsProxyAgent(PROXY_URL);
      sockConfig.agent = agent;
      sockConfig.fetchAgent = agent;
      sockConfig.options = { agent }; // Ensure ws receives the agent
    }
    log(`proxy enabled: ${redactProxyUrl(PROXY_URL)}`);
  }

  const sock = makeWASocket(sockConfig);

  sock.ev.on('creds.update', saveCreds);

  // --- Pairing Code Flow ---
  if (!sock.authState.creds.registered) {
    if (!PAIRING_PHONE) {
      writeState({ status: 'error', error: 'WA_PAIRING_PHONE not set' });
      log('pairing mode requires WA_PAIRING_PHONE env');
      process.exit(2);
    }
    setTimeout(async () => {
      try {
        const code = await sock.requestPairingCode(PAIRING_PHONE);
        writeState({
          status: 'awaiting_pairing_code',
          login_mode: 'pairing',
          code,
          phone_hint: redactPhone(PAIRING_PHONE),
        });
        log('pairing code generated; check state file');
      } catch (e) {
        writeState({ status: 'error', error: 'requestPairingCode failed: ' + e.message });
        log('pairing code request failed: ' + e.message);
      }
    }, 1500);
  }

  // --- Connection Events ---
  sock.ev.on('connection.update', async (update) => {
    const { connection, lastDisconnect } = update;

    if (connection === 'connecting') {
      writeState({ status: 'connecting', login_mode: LOGIN_MODE });
    }

    if (connection === 'open') {
      const me = sock.user || {};
      writeState({
        status: 'connected',
        login_mode: LOGIN_MODE,
        wid: redactJid(me.id || ''),
        pushname_present: !!(me.name || me.verifiedName),
      });
      log('connected');
    }

    if (connection === 'close') {
      const code = lastDisconnect?.error?.output?.statusCode;
      const reason = lastDisconnect?.error?.message || `code=${code}`;
      writeState({ status: 'disconnected', login_mode: LOGIN_MODE, reason, code });
      log(`disconnected: ${reason} (statusCode=${code})`);

      if (code === 401) {
        log('logged out (401), exiting');
        process.exit(0);
      }

      const delay = Math.min(2000 * Math.pow(2, reconnectAttempts), 30000);
      reconnectAttempts++;
      log(`reconnecting in ${delay}ms...`);
      setTimeout(() => boot().catch((e) => {
        writeState({ status: 'error', error: 'reconnect failed: ' + e.message });
        process.exit(1);
      }), delay);
    }

    if (connection === 'open') {
      reconnectAttempts = 0;
    }
  });

  // --- OTP Capture ---
  sock.ev.on('messaging-history.set', ({ messages, isLatest, syncType }) => {
    log(`messaging-history.set count=${messages.length} isLatest=${isLatest} syncType=${syncType}`);
    for (const m of messages || []) {
      _processMessage(m, 'history');
    }
  });

  sock.ev.on('messages.upsert', ({ messages, type }) => {
    log(`messages.upsert type=${type} count=${messages.length}`);
    for (const m of messages) {
      _processMessage(m, 'live');
    }
  });

  function _processMessage(m, source) {
    try {
      if (m.key?.fromMe) return;
      const { pushName, remoteJid } = extractSender(m);
      const body = extractText(m.message);

      const otpMatch = body.match(OTP_REGEX);
      log(`${source} msg from_present=${!!pushName} jid=${redactJid(remoteJid).slice(0, 40)} body_len=${body.length} has_6digit=${!!otpMatch}`);

      if (!body && m.message) {
        const types = Object.keys(m.message).filter((k) => k !== 'messageContextInfo');
        log(`  body empty, message type=[${types.join(',')}]`);
        if (types.length && types[0] !== 'protocolMessage') {
          log(`  message sample=${truncate(JSON.stringify(m.message))}`);
        }
      }

      if (!otpMatch) return;

      const senderMatch = SENDER_PATTERNS.some((re) =>
        re.test(pushName) || re.test(body) || re.test(remoteJid)
      );
      const genericMatch = GENERIC_OTP_PATTERNS.some((re) => re.test(body));
      if (!senderMatch && !genericMatch) {
        log('  6-digit number found, but sender/body did not match OTP filters');
        return;
      }

      const otp = otpMatch[1];
      deliverOtp(otp, source, remoteJid, Date.now());
      log(`OTP captured (${source}) from jid=${redactJid(remoteJid).slice(0, 30)}`);
    } catch (e) {
      console.error('[wa] _processMessage error:', e.message);
    }
  }
}

// Graceful shutdown
function cleanup() {
  log('shutting down ...');
  writeState({ status: 'shutdown' });
  process.exit(0);
}
process.on('SIGINT', cleanup);
process.on('SIGTERM', cleanup);

startGrpcServer();

boot().catch((e) => {
  writeState({ status: 'error', error: 'boot failed: ' + e.message });
  log('boot failed: ' + e.message);
  process.exit(1);
});
