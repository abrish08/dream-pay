# Accept payments with paygate

This guide shows how to take payments on your website or app: telebirr,
CBE Birr, M-Pesa, cards (Visa, Mastercard) and bank transfer, in ETB or USD.

**Pick your language:** [Node.js](nodejs.md) · [PHP](php.md) · [Python](python.md) · [Java](java.md) · [Go](go.md)

## How a payment works

```
Customer          Your website / app        Your server                paygate
   │ "Pay" ──────────▶│                          │                         │
   │                  │── create order ─────────▶│                         │
   │                  │                          │── POST /v1/payments ───▶│
   │                  │                          │◀── id, checkout_url ────│
   │◀────────── redirect to checkout_url ────────│                         │
   │─────────── pays (card + 3-D Secure, telebirr PIN, ...) ──────────────▶│
   │◀────────── back to your return_url ───────────────────────────────────│
   │                                             │◀── webhook ─────────────│
   │                                             │  payment.succeeded      │
   │                                             │  → mark order paid      │
```

1. **Your server** creates the payment with your secret API key and your order
   id as the `Idempotency-Key`.
2. You send the customer to the `checkout_url`. On a website, redirect to it.
   In a mobile app, open it in an in-app browser (Android Custom Tabs, iOS
   `SFSafariViewController`).
3. The customer pays on the payment page. Card numbers are typed on the card
   network's own page; they never reach you or paygate.
4. The customer returns to your `return_url`. **Do not mark the order paid
   yet**: show "payment is being confirmed".
5. paygate sends a signed **webhook** to your server. Verify it, then mark the
   order paid and deliver.

## Before you start

| You need | Where |
|---|---|
| A merchant account | Sign up on the dashboard and verify your email |
| A **test** API key (`sk_test_...`) | Dashboard → API keys. Works at once, no real money |
| A **live** API key (`sk_live_...`) | After your business verification (KYC) is approved |
| A webhook endpoint and its secret (`whsec_...`) | Dashboard → Webhooks, or `POST /v1/webhook-endpoints` |

**Keep the API key on your server.** Never put it in a website's JavaScript or
in a mobile app: anyone could read it.

## The basics

| | |
|---|---|
| Base URL | `https://api.paygate.example` (test and live use the same URL; the key decides) |
| Authentication | `Authorization: Bearer sk_test_...` |
| Format | JSON in, JSON out |
| Amounts | **Minor units** (cents): `5000` = 50.00 ETB or $50.00 |
| Currencies | `ETB`, `USD` |
| Idempotency | `Idempotency-Key` header is **required** on `POST /v1/payments` and `POST /v1/refunds`. Use your order id. Sending the same request again returns the same payment, so a retry after a timeout never charges twice. The same key with a different body is refused (`409 idempotency_key_reused`). |

### Create a payment

```http
POST /v1/payments
Authorization: Bearer sk_test_...
Idempotency-Key: ORDER-1001
Content-Type: application/json

{
  "amount": 5000,
  "currency": "USD",
  "payment_method": "card",
  "reference": "ORDER-1001",
  "description": "2 bags of coffee",
  "customer": {"email": "buyer@example.com", "name": "Abebe Kebede"},
  "return_url": "https://shop.example/thanks?order=ORDER-1001",
  "metadata": {"cart_id": "c_77"}
}
```

| Field | Required | Notes |
|---|---|---|
| `amount` | yes | minor units, greater than 0 |
| `currency` | yes | `ETB` or `USD` |
| `reference` | yes | your order id (max 100 characters) |
| `payment_method` | no | `telebirr`, `cbe_birr`, `mpesa`, `card`, `bank_transfer`. Leave it out to let the customer choose on paygate's page |
| `return_url` | no | where the customer goes after paying |
| `customer` | no | `email`, `name`, `phone`, `ip` (the customer's IP as your server saw it). Send what you have: fraud checks use them |
| `description` | no | shown to the customer |
| `metadata` | no | your own key/values (strings), returned in webhooks |
| `expires_in_minutes` | no | 5 to 1440, default 30 |

Response `201` (or `200` when the same `Idempotency-Key` is sent again):

```json
{
  "id": "c542aff3-772e-475f-bd5c-575c007bed47",
  "status": "processing",
  "amount": 5000, "fee": 75, "currency": "USD",
  "reference": "ORDER-1001",
  "checkout_url": "https://api.paygate.example/providers/mpgs/pay/c542aff3-...?session=...",
  "expires_at": "2026-11-02T14:30:00Z"
}
```

### Payment statuses

| Status | Meaning | What to do |
|---|---|---|
| `processing` | waiting for the customer | wait for the webhook |
| `succeeded` | paid | deliver the order |
| `failed` | declined; `failure_reason` says why (`insufficient_funds`, `declined`, `expired_card`, `authentication_failed`, ...) | let the customer try again (create a new payment) |
| `expired` | not paid within the time limit | cancel or recreate |
| `partially_refunded`, `refunded` | money (partly) returned | — |

A `failed` payment can still become `succeeded` if the customer pays with
another card on the same page: always trust the latest webhook.

## Webhooks

paygate sends a `POST` to your endpoint when something happens:

| Event | When |
|---|---|
| `payment.succeeded` | the customer paid |
| `payment.failed` | the payment was declined (`data.reason`) |
| `payment.expired` | not paid in time |
| `refund.succeeded`, `refund.failed` | a refund finished |
| `payout.paid`, `payout.failed` | a payout to your bank finished |
| `dispute.opened`, `dispute.closed` | a cardholder disputed a payment / the dispute was decided (respond in the dashboard before `evidence_due_by`) |

```http
POST https://shop.example/webhooks/paygate
Content-Type: application/json
Paygate-Event-Id: 6a1f0c2e-3b4d-4e5f-8a9b-0c1d2e3f4a5b
Paygate-Event-Type: payment.succeeded
Paygate-Signature: t=1762092000,v1=9c86b7e7f7f8c187aacf3e8dbfff446e873deacba4b2f4812f499c687e6cef72

{"id":"6a1f0c2e-...","type":"payment.succeeded","created":1762092000,
 "data":{"payment_id":"c542aff3-...","amount":5000,"fee":75,"currency":"USD","reference":"ORDER-1001",...}}
```

### Verify every webhook

Anyone can send a request to your URL, so check the signature first:

1. Read the **raw body** exactly as received. Re-encoding the JSON changes the bytes and breaks the signature.
2. From `Paygate-Signature`, take `t` (Unix time) and `v1` (hex).
3. Compute `HMAC-SHA256(secret, t + "." + rawBody)` as hex and compare it to `v1` **in constant time**.
4. Reject it if `t` is more than **5 minutes** from now (replay protection).
5. Skip events whose `id` you already processed: the same event can arrive more than once.
6. Answer `2xx` quickly. Any other answer, or no answer in 10 seconds, is retried
   up to 8 times (30 s, 1 min, 2 min, ... up to 6 h).

Each language page has a ready function for this. To check your own
implementation, it must accept this exact example
([testdata/webhook-vector.json](testdata/webhook-vector.json)):

```
secret:    whsec_test_5f3c1a9e7b2d4c6e8a0f1b3d5e7a9c1e
t:         1762092000
signature: 9c86b7e7f7f8c187aacf3e8dbfff446e873deacba4b2f4812f499c687e6cef72
body:      (the "body" string in the file, byte for byte)
```

**Rotating the secret:** `POST /v1/webhook-endpoints/{id}/rotate-secret` returns
a new secret; deliveries are signed with it at once. Accept both the old and
the new secret for a few minutes while you deploy the change.

## Errors

Every error has the same shape:

```json
{"error": {"kind": "unprocessable", "code": "payment_method_unavailable", "message": "card is not available for ETB"}}
```

| HTTP | `kind` | Common `code` | What to do |
|---|---|---|---|
| 400 | `invalid` | `invalid_amount`, `unsupported_currency`, `idempotency_key_required` | fix the request |
| 401 | `unauthorized` | `invalid_api_key` | check the key |
| 403 | `forbidden` | `kyc_required`, `permission_denied` | finish verification / use a key with the needed scope |
| 404 | `not_found` | `payment_not_found` | — |
| 409 | `conflict` | `idempotency_key_reused` | use a new key for a different request |
| 422 | `unprocessable` | `payment_method_unavailable`, `refund_exceeds_payment` | offer another method / smaller amount |
| 422 | `unprocessable` | `risk_declined` | the payment was declined by fraud checks; do not retry it as is |
| 422 | `unprocessable` | `limit_exceeded` | above your account's limits; ask us to raise them |
| 429 | `too_many_requests` | `rate_limited` | retry after a short wait |
| 5xx | `internal`, `unavailable` | — | retry with the **same** Idempotency-Key |

The libraries on the language pages already retry network errors, `429` and
`5xx` (safe thanks to the Idempotency-Key) and raise a typed error otherwise.

## Other calls

| Call | Use |
|---|---|
| `GET /v1/payments/{id}` | current status (e.g. if a webhook was missed) |
| `POST /v1/payment-links` `{amount, currency, description, single_use}` | a URL to share instead of integrating (invoices, social media, a QR at the counter) |
| `GET /v1/reports/payments.csv`, `/v1/reports/statement.csv` | exports for accounting |
| `GET /v1/payments/{id}/events` | the payment's full history: every status change, who caused it and why |
| `GET /v1/payments?reference=ORDER-1001&status=succeeded&limit=20` | search; page with `starting_after=<last id>` while `has_more` is true |
| `POST /v1/refunds` `{"payment_id", "amount"?, "reason"?}` | refund all (no amount) or part |
| `GET /v1/balance?currency=USD` | what you have earned (after fees) |
| `GET /v1/payment-methods?currency=USD` | methods you can offer right now |

The full reference with every field is in the API docs (`/docs`, or `/swagger`
to try requests).

## Testing

With a **test** key nothing is charged. When the gateway has a card TEST
account, card payments in test mode open the card network's test page:

| Card | Expiry | Result |
|---|---|---|
| `5123 4500 0000 0008` (Mastercard) or `4508 7500 1574 1019` (Visa), any CVV | `01/39` | approved |
| | `02/39` | `insufficient_funds` |
| | `05/39` | `declined` |
| | `04/27` | `expired_card` |
| 3-D Secure code | `123456` | other codes fail with `authentication_failed` |

Other methods (telebirr, CBE Birr, ...) show a test page with **Pay** and
**Decline** buttons. Two test amounts simulate provider trouble:

| Amount ends in | Simulates | You see |
|---|---|---|
| `…9.99` (e.g. `20999`) | the provider got the request but did not answer | `201`, status `processing`, no `checkout_url`; the result arrives by webhook once confirmed |
| `…9.98` (e.g. `10998`) | the provider is unreachable | `502 provider_unavailable`; the payment is `failed` |

Handle the first case like a real telebirr push: show "waiting for
confirmation" and wait for the webhook. **Never** treat `processing` as failed.

## Going live checklist

- [ ] Business verification (KYC) approved, live key created
- [ ] Live key stored as a server secret (environment variable / secret manager), never in code or in the app
- [ ] Webhook endpoint on **https**, signature verified, duplicate events ignored
- [ ] Orders are marked paid **only** from the webhook (or `GET /v1/payments/{id}`), never from the return URL alone
- [ ] `Idempotency-Key` = your order id on every payment and refund
- [ ] Amounts sent in minor units
- [ ] Errors logged with `code`; customers see a friendly message
- [ ] One real low-value payment and refund made end to end
