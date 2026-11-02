# Node.js integration

Node.js 18 or newer. No dependencies: the client uses the built-in `fetch`
and `crypto`. Read [How a payment works](README.md#how-a-payment-works) first.

## 1. Add the client

Copy [`paygate.js`](../../examples/integration/node/paygate.js) into your
project and configure it once:

```js
const { Paygate, PaygateError, verifyWebhook } = require('./paygate');

const paygate = new Paygate(process.env.PAYGATE_API_KEY, {
  baseUrl: process.env.PAYGATE_BASE_URL, // https://api.paygate.example
});
```

```bash
# .env of your server (never commit it, never ship it to the browser)
PAYGATE_API_KEY=sk_test_...
PAYGATE_WEBHOOK_SECRET=whsec_...
PAYGATE_BASE_URL=https://api.paygate.example
```

## 2. Create a payment and redirect the customer

```js
// Express
app.post('/checkout', async (req, res) => {
  const order = await db.orders.create({ total: 5000, status: 'pending' });
  try {
    const payment = await paygate.createPayment({
      amount: order.total,              // minor units: 5000 = 50.00
      currency: 'ETB',
      payment_method: 'telebirr',       // or 'card', 'cbe_birr', ...; leave out to let the customer choose
      reference: order.id,
      description: 'Order ' + order.id,
      customer: { email: req.user.email },
      return_url: `https://shop.example/thanks?order=${order.id}`,
    }, order.id);                       // Idempotency-Key = your order id
    await db.orders.update(order.id, { paymentId: payment.id });
    res.redirect(303, payment.checkout_url);
  } catch (err) {
    if (err instanceof PaygateError) console.error(err.status, err.code, err.message);
    res.status(502).send('Payment is unavailable, please try again.');
  }
});
```

## 3. The customer comes back

```js
app.get('/thanks', async (req, res) => {
  const order = await db.orders.find(req.query.order);
  // Do not mark it paid here: the webhook does that.
  res.render('thanks', { paid: order.status === 'paid' }); // "Payment is being confirmed..." until then
});
```

## 4. Receive webhooks

The signature is computed on the **raw body**, so read it before any JSON parser:

```js
app.post('/webhooks/paygate', express.raw({ type: 'application/json' }), async (req, res) => {
  let event;
  try {
    event = verifyWebhook(req.body, req.get('Paygate-Signature'), process.env.PAYGATE_WEBHOOK_SECRET);
  } catch {
    return res.status(400).send('invalid signature');
  }
  if (await db.events.exists(event.id)) return res.sendStatus(200); // already handled
  await db.events.insert(event.id);

  const orderId = event.data.reference;
  switch (event.type) {
    case 'payment.succeeded': await db.orders.update(orderId, { status: 'paid' }); break;
    case 'payment.failed':    await db.orders.update(orderId, { status: 'failed', reason: event.data.reason }); break;
    case 'payment.expired':   await db.orders.update(orderId, { status: 'expired' }); break;
  }
  res.sendStatus(200); // answer quickly; do slow work in a background job
});
```

Register this route **before** `app.use(express.json())`, or the raw body is lost.
While rotating the secret pass both: `verifyWebhook(body, header, [oldSecret, newSecret])`.

## 5. Status, refunds, balance

```js
const p = await paygate.getPayment(paymentId);            // p.status, p.failure_reason
const page = await paygate.listPayments({ reference: 'ORDER-1001', limit: 20 });
const refund = await paygate.createRefund(
  { payment_id: paymentId, amount: 2000, reason: 'one item returned' }, // omit amount = refund the rest
  `refund-${orderId}-1`);                                   // unique key per refund
const balance = await paygate.getBalance('ETB');           // balance.available
```

## Errors

```js
try {
  await paygate.createPayment(params, orderId);
} catch (err) {
  if (!(err instanceof PaygateError)) throw err;
  switch (err.code) {
    case 'payment_method_unavailable': /* offer another method */ break;
    case 'kyc_required':               /* live mode: finish business verification */ break;
    case 'idempotency_key_reused':     /* same order id, different amount: use a new key */ break;
    default: console.error(err.status, err.code, err.message);
  }
}
```

Network errors, `429` and `5xx` are retried automatically (2 retries) with the
same Idempotency-Key, so they never create a second payment.

## Test it

```bash
cd examples/integration/node
node --test                                   # webhook verification
PAYGATE_BASE_URL=http://localhost:8080 PAYGATE_API_KEY=sk_test_... node --test   # also the live API
```

A complete runnable shop (checkout, thank-you page, webhook) is in
[`server.js`](../../examples/integration/node/server.js).
