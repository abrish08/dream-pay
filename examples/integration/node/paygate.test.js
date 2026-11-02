// node --test   (live tests need PAYGATE_BASE_URL and PAYGATE_API_KEY with a test key)
'use strict';
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');
const { Paygate, PaygateError, verifyWebhook } = require('./paygate');

const v = JSON.parse(fs.readFileSync(path.join(__dirname, '../../../docs/integration/testdata/webhook-vector.json')));

test('verifies the published webhook vector', () => {
  const e = verifyWebhook(v.body, v.header, v.secret, 300, v.timestamp + 10);
  assert.equal(e.type, 'payment.succeeded');
  assert.equal(e.data.reference, 'ORDER-1001');
});
test('accepts the new secret while rotating', () => {
  assert.ok(verifyWebhook(v.body, v.header, ['whsec_old', v.secret], 300, v.timestamp));
});
test('rejects tampering, wrong secret, old timestamp, bad header', () => {
  assert.throws(() => verifyWebhook(v.body.replace('5000', '9000'), v.header, v.secret, 300, v.timestamp), /mismatch/);
  assert.throws(() => verifyWebhook(v.body, v.header, 'whsec_wrong', 300, v.timestamp), /mismatch/);
  assert.throws(() => verifyWebhook(v.body, v.header, v.secret, 300, v.timestamp + 301), /tolerance/);
  assert.throws(() => verifyWebhook(v.body, 'garbage', v.secret, 300, v.timestamp), /malformed/);
});

const live = process.env.PAYGATE_BASE_URL && process.env.PAYGATE_API_KEY;
test('live: create, idempotent retry, get, list, errors', { skip: !live && 'set PAYGATE_BASE_URL and PAYGATE_API_KEY' }, async () => {
  const pg = new Paygate(process.env.PAYGATE_API_KEY, { baseUrl: process.env.PAYGATE_BASE_URL });
  const ref = `NODE-${Date.now()}`;
  const params = { amount: 5000, currency: 'USD', payment_method: 'card', reference: ref };
  const p = await pg.createPayment(params, ref);
  assert.match(p.checkout_url, /^http/);
  assert.equal((await pg.createPayment(params, ref)).id, p.id, 'same key returns the same payment');
  assert.equal((await pg.getPayment(p.id)).reference, ref);
  assert.ok((await pg.listPayments({ reference: ref })).data.some((x) => x.id === p.id));
  await assert.rejects(pg.createPayment({ ...params, amount: 1 }, ref), (e) => e instanceof PaygateError && e.code === 'idempotency_key_reused');
  await assert.rejects(pg.getPayment('00000000-0000-0000-0000-000000000000'), (e) => e.status === 404);
  await assert.rejects(new Paygate('sk_test_wrong', { baseUrl: process.env.PAYGATE_BASE_URL }).getBalance(), (e) => e.status === 401);
});
