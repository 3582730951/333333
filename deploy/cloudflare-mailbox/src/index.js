const encoder = new TextEncoder();

function json(value, status = 200, extraHeaders = {}) {
  return new Response(JSON.stringify(value), {
    status,
    headers: {
      'content-type': 'application/json; charset=utf-8',
      'cache-control': 'no-store',
      ...extraHeaders,
    },
  });
}

function base64url(bytes) {
  let binary = '';
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/u, '');
}

function decodeBase64url(value) {
  const normalized = value.replaceAll('-', '+').replaceAll('_', '/');
  const padded = normalized + '='.repeat((4 - (normalized.length % 4)) % 4);
  const binary = atob(padded);
  return Uint8Array.from(binary, (character) => character.charCodeAt(0));
}

async function hmac(secret, value) {
  const key = await crypto.subtle.importKey(
    'raw', encoder.encode(secret), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign', 'verify'],
  );
  return new Uint8Array(await crypto.subtle.sign('HMAC', key, encoder.encode(value)));
}

export async function signMailboxToken(secret, address, expiresAt) {
  const header = base64url(encoder.encode(JSON.stringify({ alg: 'HS256', typ: 'JWT' })));
  const payload = base64url(encoder.encode(JSON.stringify({ sub: address, address, exp: expiresAt })));
  const signingInput = `${header}.${payload}`;
  return `${signingInput}.${base64url(await hmac(secret, signingInput))}`;
}

export async function verifyMailboxToken(secret, token, now = Math.floor(Date.now() / 1000)) {
  if (!secret || typeof token !== 'string') return null;
  const parts = token.split('.');
  if (parts.length !== 3) return null;
  try {
    const expected = await hmac(secret, `${parts[0]}.${parts[1]}`);
    const supplied = decodeBase64url(parts[2]);
    if (expected.length !== supplied.length) return null;
    let difference = 0;
    for (let index = 0; index < expected.length; index += 1) difference |= expected[index] ^ supplied[index];
    if (difference !== 0) return null;
    const payload = JSON.parse(new TextDecoder().decode(decodeBase64url(parts[1])));
    if (typeof payload?.address !== 'string' || !Number.isFinite(payload?.exp) || payload.exp <= now) return null;
    return payload;
  } catch {
    return null;
  }
}

export function normalizeLocalPart(value) {
  const local = String(value || '').trim().toLowerCase();
  if (!/^[a-z0-9][a-z0-9._-]{0,62}[a-z0-9]$/u.test(local) && !/^[a-z0-9]$/u.test(local)) {
    throw new Error('name must contain only letters, digits, dot, underscore, or hyphen');
  }
  if (local.includes('..')) throw new Error('name must not contain consecutive dots');
  return local;
}

function randomLocalPart() {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  return `box-${Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('')}`;
}

function positiveInteger(value, fallback, maximum) {
  const parsed = Number.parseInt(String(value || ''), 10);
  return Number.isFinite(parsed) && parsed > 0 ? Math.min(parsed, maximum) : fallback;
}

function mailboxDomain(env) {
  return String(env.MAIL_DOMAIN || '').trim().toLowerCase().replace(/^@/u, '');
}

async function cleanup(env, now) {
  await env.DB.batch([
    env.DB.prepare('DELETE FROM messages WHERE expires_at <= ?').bind(now),
    env.DB.prepare('DELETE FROM mailboxes WHERE expires_at <= ?').bind(now),
  ]);
}

async function createMailbox(request, env, adminRoute) {
  if (adminRoute) {
    const supplied = request.headers.get('x-admin-auth') || '';
    if (!env.ADMIN_TOKEN || supplied !== env.ADMIN_TOKEN) return json({ error: 'admin authentication failed' }, 401);
  } else if (String(env.ALLOW_PUBLIC_CREATE || '').toLowerCase() !== 'true') {
    return json({ error: 'public address creation is disabled; use /admin/new_address' }, 403);
  }
  if (!env.DB || !env.JWT_SECRET) return json({ error: 'worker is missing DB or JWT_SECRET' }, 503);

  let body;
  try {
    body = await request.json();
  } catch {
    return json({ error: 'request body must be JSON' }, 400);
  }
  const domain = mailboxDomain(env);
  if (!domain || !/^[a-z0-9.-]+\.[a-z]{2,}$/u.test(domain)) return json({ error: 'MAIL_DOMAIN is invalid' }, 503);
  let local;
  try {
    local = body?.name ? normalizeLocalPart(body.name) : randomLocalPart();
  } catch (error) {
    return json({ error: error.message }, 400);
  }
  const address = `${local}@${domain}`;
  const now = Math.floor(Date.now() / 1000);
  const expiresAt = now + positiveInteger(env.MAILBOX_TTL_SECONDS, 86400, 604800);
  await env.DB.prepare(`
    INSERT INTO mailboxes(address,created_at,expires_at) VALUES(?,?,?)
    ON CONFLICT(address) DO UPDATE SET expires_at=excluded.expires_at
  `).bind(address, now, expiresAt).run();
  const jwt = await signMailboxToken(env.JWT_SECRET, address, expiresAt);
  return json({ jwt, address, name: local, expires_at: expiresAt }, 201);
}

async function listMails(request, env) {
  const authorization = request.headers.get('authorization') || '';
  const token = authorization.toLowerCase().startsWith('bearer ') ? authorization.slice(7).trim() : '';
  const payload = await verifyMailboxToken(env.JWT_SECRET, token);
  if (!payload) return json({ error: 'mailbox token is invalid or expired' }, 401);
  const url = new URL(request.url);
  const limit = positiveInteger(url.searchParams.get('limit'), 20, 100);
  const offset = Math.max(0, Number.parseInt(url.searchParams.get('offset') || '0', 10) || 0);
  const now = Math.floor(Date.now() / 1000);
  const result = await env.DB.prepare(`
    SELECT id,sender,subject,raw,created_at
    FROM messages
    WHERE recipient=? AND expires_at>?
    ORDER BY created_at DESC LIMIT ? OFFSET ?
  `).bind(payload.address.toLowerCase(), now, limit, offset).all();
  const results = (result.results || []).map((message) => ({
    id: message.id,
    sender: message.sender,
    subject: message.subject,
    raw: message.raw,
    text: message.raw,
    message: message.raw,
    created_at: message.created_at,
  }));
  return json({ results, address: payload.address, limit, offset });
}

async function handleFetch(request, env, ctx) {
  const url = new URL(request.url);
  if (request.method === 'GET' && url.pathname === '/healthz') {
    return json({ ok: true, adapter: 'cloudflare_temp_email', domain: mailboxDomain(env) });
  }
  if (request.method === 'POST' && url.pathname === '/admin/new_address') return createMailbox(request, env, true);
  if (request.method === 'POST' && url.pathname === '/api/new_address') return createMailbox(request, env, false);
  if (request.method === 'GET' && url.pathname === '/api/mails') return listMails(request, env);
  if (request.method === 'OPTIONS') return new Response(null, { status: 204 });
  if (ctx?.waitUntil && env.DB) ctx.waitUntil(cleanup(env, Math.floor(Date.now() / 1000)));
  return json({ error: 'not found' }, 404);
}

async function handleEmail(message, env, ctx) {
  if (!env.DB) return;
  const recipient = String(message.to || '').trim().toLowerCase();
  const domain = mailboxDomain(env);
  if (!recipient.endsWith(`@${domain}`)) return;
  const now = Math.floor(Date.now() / 1000);
  const mailbox = await env.DB.prepare(
    'SELECT address FROM mailboxes WHERE address=? AND expires_at>?',
  ).bind(recipient, now).first();
  if (!mailbox) return;
  const raw = await new Response(message.raw).text();
  const subject = String(message.headers?.get('subject') || '').slice(0, 998);
  const sender = String(message.from || '').slice(0, 320);
  const expiresAt = now + positiveInteger(env.MESSAGE_TTL_SECONDS, 86400, 604800);
  await env.DB.prepare(`
    INSERT INTO messages(id,recipient,sender,subject,raw,created_at,expires_at)
    VALUES(?,?,?,?,?,?,?)
  `).bind(crypto.randomUUID(), recipient, sender, subject, raw, now, expiresAt).run();
  if (ctx?.waitUntil) ctx.waitUntil(cleanup(env, now));
}

export default { fetch: handleFetch, email: handleEmail };
