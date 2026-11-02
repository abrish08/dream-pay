// paygate.js: Node.js 18+ client for the paygate API. No dependencies.
'use strict';
const crypto = require('node:crypto');

class PaygateError extends Error {
  constructor(status, body) {
    const e = (body && body.error) || {};
    super(e.message || `paygate request failed with HTTP ${status}`);
    this.name = 'PaygateError';
    this.status = status;       // HTTP status, 0 = network error
    this.kind = e.kind || '';   // invalid | unauthorized | forbidden | not_found | conflict | unprocessable | ...
    this.code = e.code || '';   // e.g. idempotency_key_reused, kyc_required, payment_method_unavailable
  }
}

class Paygate {
  /**
   * @param {string} apiKey  sk_test_... or sk_live_... (server side only, never in a browser or app)
   * @param {{baseUrl?: string, timeoutMs?: number, maxRetries?: number}} [opts]
   */
  constructor(apiKey, opts = {}) {
    if (!apiKey) throw new Error('paygate: API key is required');
    this.apiKey = apiKey;
    this.baseUrl = (opts.baseUrl || 'https://api.paygate.example').replace(/\/+$/, '');
    this.timeoutMs = opts.timeoutMs || 30000;
    this.maxRetries = opts.maxRetries ?? 2;
  }

  // ---- Payments ----
  /** Amount in minor units (cents): 5000 = 50.00. idempotencyKey: your order id. */
  createPayment(params, idempotencyKey) {
    return this._request('POST', '/v1/payments', params, requireKey(idempotencyKey));
  }
  getPayment(id) { return this._request('GET', `/v1/payments/${encodeURIComponent(id)}`); }
  /** query: {status, reference, limit, starting_after, created_from, created_to} */
  listPayments(query = {}) { return this._request('GET', '/v1/payments' + qs(query)); }

  // ---- Refunds ----
  /** params: {payment_id, amount? (omit = the rest), reason?} */
  createRefund(params, idempotencyKey) {
    return this._request('POST', '/v1/refunds', params, requireKey(idempotencyKey));
  }
  getRefund(id) { return this._request('GET', `/v1/refunds/${encodeURIComponent(id)}`); }

  // ---- Balance ----
  getBalance(currency = 'ETB') { return this._request('GET', '/v1/balance' + qs({ currency })); }

  async _request(method, path, body, idempotencyKey) {
    const headers = { Authorization: `Bearer ${this.apiKey}`, Accept: 'application/json' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    if (idempotencyKey) headers['Idempotency-Key'] = idempotencyKey;
    // Safe to retry: GETs, and POSTs with an Idempotency-Key (the same key never charges twice).
    for (let attempt = 0; ; attempt++) {
      let res;
      try {
        res = await fetch(this.baseUrl + path, {
          method, headers, body: body === undefined ? undefined : JSON.stringify(body),
          signal: AbortSignal.timeout(this.timeoutMs),
        });
      } catch (err) {
        if (attempt < this.maxRetries) { await sleep(backoff(attempt)); continue; }
        throw Object.assign(new PaygateError(0, { error: { message: `network error: ${err.message}` } }), { cause: err });
      }
      const text = await res.text();
      const data = text ? JSON.parse(text) : {};
      if (res.ok) return data;
      if ((res.status >= 500 || res.status === 429) && attempt < this.maxRetries) { await sleep(backoff(attempt)); continue; }
      throw new PaygateError(res.status, data);
    }
  }
}

/**
 * Verify a webhook and return the event. Throws if it is not from paygate.
 * @param {Buffer|string} rawBody  the body exactly as received (not re-serialized JSON)
 * @param {string} header          the Paygate-Signature header: "t=<unix>,v1=<hex>"
 * @param {string|string[]} secrets the endpoint secret(s) (several while rotating)
 * @param {number} [toleranceSec]  max age of the signature (replay protection)
 */
function verifyWebhook(rawBody, header, secrets, toleranceSec = 300, now = Date.now() / 1000) {
  const parts = Object.fromEntries(String(header || '').split(',').map((p) => p.trim().split('=', 2)));
  const t = Number(parts.t);
  if (!Number.isInteger(t) || !/^[0-9a-f]{64}$/.test(parts.v1 || '')) throw new Error('paygate: malformed signature header');
  if (Math.abs(now - t) > toleranceSec) throw new Error('paygate: signature timestamp outside tolerance');
  const body = Buffer.isBuffer(rawBody) ? rawBody : Buffer.from(rawBody, 'utf8');
  const given = Buffer.from(parts.v1, 'hex');
  const ok = [].concat(secrets).some((secret) => {
    const expected = crypto.createHmac('sha256', secret).update(`${t}.`).update(body).digest();
    return crypto.timingSafeEqual(expected, given);
  });
  if (!ok) throw new Error('paygate: signature mismatch');
  return JSON.parse(body.toString('utf8'));
}

function requireKey(k) {
  if (!k) throw new Error('paygate: an Idempotency-Key is required (use your order id)');
  return k;
}
function qs(q) {
  const s = new URLSearchParams(Object.entries(q).filter(([, v]) => v !== undefined && v !== '')).toString();
  return s ? `?${s}` : '';
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const backoff = (attempt) => 300 * 2 ** attempt + Math.floor(Math.random() * 100);

module.exports = { Paygate, PaygateError, verifyWebhook };
