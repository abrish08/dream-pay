# How customers pay

[← back to README](../README.md)

## Where the money comes from

| Method | Customer pays from | Approves with | Adapter today |
|---|---|---|---|
| `telebirr`, `cbe_birr`, `mpesa` | their mobile wallet | PIN prompt on the phone | **placeholder** (mock wallet API) |
| `card` | their bank card (Visa, Mastercard) | 3-D Secure on Mastercard's page | Mastercard Gateway (MPGS) |
| `bank_transfer` | their bank account | their own bank app / branch | **manual bank transfer** (works now) |

## The checkout page (`/pay/{id}`)

English and Amharic (`?lang=am`, remembered; or the browser's language).

```
choose method ─▶ wallet: phone number ─▶ "check your phone, enter your PIN" (live status) ─▶ success / failure
              ─▶ card: Mastercard's page (card + 3-D Secure) ─────────────────────────────▶ success / failure
              ─▶ bank transfer: accounts + reference to write ─▶ confirmed by finance ───▶ success
```

* If the merchant sends `customer.phone` (normalised to `+251…`), the push
  starts at once; otherwise the page asks for it.
* The page polls `GET /pay/{id}/status` and updates by itself.
* Failures are explained in plain words (insufficient funds, declined on the
  phone, not approved in time, card expired, verification failed, time ran
  out), with a button back to the merchant.

## One payment, one charge

| Situation | Protection |
|---|---|
| Merchant server retries the create | `Idempotency-Key` (unique in the database) |
| Customer double-clicks, refreshes, opens two tabs | the payment is locked while starting; a started payment returns the same checkout |
| Two requests start the provider at the same moment | the provider attempt is recorded first (unique per payment): only one is sent |
| Provider times out after the request | not retried, not sent elsewhere: status checks ask the provider |
| Provider unreachable before accepting | safe to fail over to the next provider |
| Single-use payment link visited twice | the link is locked: the same payment is reused |
| Wallet callback delivered twice | verified with the wallet; an unchanged status is ignored |

## When a payment fails

* `failed` with `failure_reason` (e.g. `insufficient_funds`,
  `declined_by_customer`, `customer_did_not_respond`); webhook
  `payment.failed`; the customer sees why.
* A failed payment is final: the merchant creates a new one (a new intent) to
  let the customer try again. If a provider later confirms that money **was**
  taken, the payment becomes `succeeded` (the capture is honoured and
  recorded in its history) and the merchant may refund it.
* No answer is never treated as failed: see "One payment, one charge".

## Bank transfer (manual, available now)

1. `BANK_TRANSFER_ACCOUNTS="Commercial Bank of Ethiopia|1000123456789|Paygate PLC;Dashen Bank|5012345678901|Paygate PLC"`
   and `PROVIDER_ROUTES=bank_transfer=bank_transfer`.
2. The customer sees the accounts, the exact amount and a reference
   (`PG3F9A1C0B27`) to write on the transfer.
3. Finance finds it on the bank statement:
   `GET /admin/bank-transfers?reference=PG3F9A1C0B27`, then
   `POST /admin/bank-transfers/{payment id}/confirm {amount, bank_reference}`.
   The amount must match exactly; a partial or extra transfer is settled with
   the customer by finance, never guessed.
4. Give bank-transfer payments enough time: `expires_in_minutes` up to 1440.
5. Refunds of bank transfers are sent back by finance (the refund is recorded
   as `manual_refund_required`).

## Wallet adapters (placeholders)

`internal/modules/provider/internal/gateway/wallet.go` speaks a generic push
API implemented by the mock wallet (`go run ./cmd/wallet-sim`, phone page at
`http://127.0.0.1:9200/phone`). When telebirr, CBE Birr or M-Pesa give you
their API, replace only the parts marked **PROVIDER-SPECIFIC** (request and
response formats, authentication, callback signature) and keep the rules:
idempotent per payment, "unavailable" only when nothing was sent, "unknown"
after a timeout, verify before trusting a callback. The app logs a warning
while a placeholder is configured.

```
TELEBIRR_TEST_BASE_URL=http://127.0.0.1:9200  TELEBIRR_TEST_API_KEY=sim-key  TELEBIRR_TEST_WEBHOOK_SECRET=sim-secret
PROVIDER_TEST_ROUTES=telebirr=telebirr_test
```

## Receipts

Email (if `customer.email`) and SMS (if a phone) in English and Amharic,
when a payment succeeds and when a refund succeeds. Sent once: the event is
marked handled before the message goes out, so a redelivered event never
sends a second receipt.
