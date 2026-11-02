// Example shop: "Buy" → paygate checkout → webhook marks the order paid.
// Run: PAYGATE_API_KEY=sk_test_... PAYGATE_WEBHOOK_SECRET=whsec_... PAYGATE_BASE_URL=http://localhost:8080 node server.js
'use strict';
const http = require('node:http');
const { Paygate, PaygateError, verifyWebhook } = require('./paygate');

const paygate = new Paygate(process.env.PAYGATE_API_KEY, { baseUrl: process.env.PAYGATE_BASE_URL });
const SHOP = process.env.SHOP_URL || 'http://localhost:3000';
const orders = new Map(); // use your database

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, SHOP);
  try {
    // 1. The customer clicks "Buy": create the payment on the server, redirect to checkout_url.
    if (req.method === 'POST' && url.pathname === '/checkout') {
      const orderId = `ORDER-${Date.now()}`;
      orders.set(orderId, { status: 'pending' });
      const payment = await paygate.createPayment({
        amount: 5000, currency: 'USD', payment_method: 'card', // or leave out: customer chooses
        reference: orderId, description: '2 bags of coffee',
        customer: { email: 'buyer@example.com' },
        return_url: `${SHOP}/thanks?order=${orderId}`,
        metadata: { order_id: orderId },
      }, orderId); // idempotency key = order id: a retry never creates a second payment
      orders.get(orderId).paymentId = payment.id;
      res.writeHead(303, { Location: payment.checkout_url }).end();
      return;
    }
    // 2. The customer is back. Show "processing" until the webhook says paid.
    if (req.method === 'GET' && url.pathname === '/thanks') {
      const order = orders.get(url.searchParams.get('order'));
      res.end(order && order.status === 'paid' ? 'Thank you, payment received.' : 'Payment is being confirmed…');
      return;
    }
    // 3. Webhook: verify the signature on the RAW body, then update the order once.
    if (req.method === 'POST' && url.pathname === '/webhooks/paygate') {
      const raw = await readBody(req);
      let event;
      try {
        event = verifyWebhook(raw, req.headers['paygate-signature'], process.env.PAYGATE_WEBHOOK_SECRET);
      } catch {
        res.writeHead(400).end('invalid signature');
        return;
      }
      const order = orders.get(event.data.reference);
      if (order && !order.events?.has(event.id)) { // events can arrive more than once
        (order.events ??= new Set()).add(event.id);
        if (event.type === 'payment.succeeded') order.status = 'paid';
        if (event.type === 'payment.failed') order.status = `failed: ${event.data.reason}`;
        if (event.type === 'payment.expired') order.status = 'expired';
      }
      res.writeHead(200).end('ok'); // answer 2xx quickly, or paygate retries
      return;
    }
    res.writeHead(404).end();
  } catch (err) {
    console.error(err instanceof PaygateError ? `${err.status} ${err.code}: ${err.message}` : err);
    res.writeHead(502).end('payment service error');
  }
});

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c)).on('end', () => resolve(Buffer.concat(chunks))).on('error', reject);
  });
}

if (require.main === module) server.listen(3000, () => console.log(`shop on ${SHOP}`));
module.exports = { server, orders };
