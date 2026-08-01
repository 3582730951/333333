import test from 'node:test';
import assert from 'node:assert/strict';

import worker, { normalizeLocalPart, signMailboxToken, verifyMailboxToken } from '../src/index.js';

test('mailbox JWT round-trips and rejects tampering or expiry', async () => {
  const token = await signMailboxToken('fixture-secret', 'box-1@example.test', 2_000_000_000);
  assert.equal((await verifyMailboxToken('fixture-secret', token, 1_900_000_000))?.address, 'box-1@example.test');
  assert.equal(await verifyMailboxToken('wrong-secret', token, 1_900_000_000), null);
  assert.equal(await verifyMailboxToken('fixture-secret', token, 2_000_000_000), null);
});

test('local part normalization accepts the adapter shape and rejects header-like input', () => {
  assert.equal(normalizeLocalPart(' Box-123 '), 'box-123');
  assert.throws(() => normalizeLocalPart('a@b.example'));
  assert.throws(() => normalizeLocalPart('x\r\nbcc'));
});

test('health contract identifies the repository adapter', async () => {
  const response = await worker.fetch(new Request('https://mailbox.example/healthz'), { MAIL_DOMAIN: 'example.test' }, {});
  assert.equal(response.status, 200);
  assert.deepEqual(await response.json(), { ok: true, adapter: 'cloudflare_temp_email', domain: 'example.test' });
});

test('public creation is closed unless explicitly enabled', async () => {
  const response = await worker.fetch(new Request('https://mailbox.example/api/new_address', {
    method: 'POST', body: '{}', headers: { 'content-type': 'application/json' },
  }), { ALLOW_PUBLIC_CREATE: 'false' }, {});
  assert.equal(response.status, 403);
});
