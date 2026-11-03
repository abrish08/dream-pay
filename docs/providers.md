# Payment providers and card payments

[← back to README](../README.md)

## Payment providers: hybrid model (direct + backup)

Merchants ask for a **payment method**, never a provider:

```
POST /v1/payments {"payment_method": "telebirr", ...}     -> routed, checkout_url = provider page
POST /v1/payments {...no payment_method...}               -> checkout_url = /pay/{id} (our hosted page,
                                                             the customer picks a method)
```

The provider module routes each method through `PROVIDER_ROUTES`
(`telebirr=telebirr,backup_x;card=ethswitch`): the first healthy provider
is used; when one is down (timeouts, 5xx) the next is tried, and a provider
that keeps failing is skipped for 30 s (circuit breaker). Declines are never
retried elsewhere. `GET /v1/payment-methods` tells merchants what is
available; `GET /admin/providers` shows staff each adapter and its health.

This is the hybrid path: start a method on whatever connection is signed
first, then move it to a direct integration by changing one line of
`PROVIDER_ROUTES`. Merchants and their code never change.

**Installed adapters:** the sandbox (test mode, every method) and the
Mastercard Gateway for cards (`mpgs` / `mpgs_test`, see below). Other live
methods report `payment_method_unavailable` until a direct adapter is added. Each adapter is one file implementing `gateway.Gateway`
(`internal/modules/provider/internal/gateway/gateway.go` lists the rules) and
needs the provider's API specification and test credentials. What to request
from each provider: [docs/PROVIDER_ONBOARDING.md](PROVIDER_ONBOARDING.md).

## Card payments in USD: Mastercard Gateway (MPGS)

Card payments (Visa, Mastercard) go through **Mastercard Payment Gateway
Services**, which Ethiopian acquiring banks give to their e-commerce
merchants. paygate uses **Hosted Checkout**: the customer types the card on
Mastercard's page, so card numbers never reach paygate (PCI DSS SAQ A).

```
POST /v1/payments {"amount": 5000, "currency": "USD", "payment_method": "card", "reference": "...", "return_url": "..."}
  → paygate: INITIATE_CHECKOUT at MPGS (order id = payment id, PURCHASE)
  → checkout_url = /providers/mpgs/pay/{id}?session=...   (loads Mastercard's checkout.min.js)
customer: card + 3-D Secure on Mastercard's page
  → /providers/mpgs/return/{id}: paygate reads the order from MPGS (never trusts the URL)
  → payment succeeded → ledger: merchant USD balance +49.25, fee 0.75 → webhook payment.succeeded
  → customer redirected to return_url
MPGS webhook → /providers/mpgs/callback (X-Notification-Secret) → same verification
POST /v1/refunds → REFUND at MPGS (transaction id = refund id: never refunded twice)
```

* **Money:** the customer's bank charges the card in USD, MPGS and the
  acquiring bank settle USD to the merchant account the bank holds (per the
  bank's settlement terms and NBE rules); paygate's ledger keeps USD separate
  from ETB, and payouts of USD go to a USD account.
* **Safety:** the order status and amount are always read from MPGS; an amount
  or currency mismatch fails the payment. Webhooks need the shared secret.
  Authentication or network errors count as "provider down" (failover and
  circuit breaker); a pending refund stays pending and is checked again.
  The card page has its own strict CSP (only Mastercard's host and our
  nonce-tagged script).
* **Test vs live:** `mpgs` is the bank's live merchant, used only by live
  payments (`PROVIDER_ROUTES=card=mpgs`). `mpgs_test` is the bank's TEST
  merchant, used only by test mode (`PROVIDER_TEST_ROUTES=card=mpgs_test`),
  where Mastercard's test cards work. The router never mixes them.
* **What to get from the bank:** card acquiring agreement for e-commerce
  (USD, and ETB if offered), the MPGS gateway host, a TEST and a live merchant
  id with API passwords (Merchant Administration → Integration Settings), the
  API version they enable, a webhook secret, and approval of your return URL
  domain. Then set the `MPGS_*` variables (see `.env.example`).

### Going live with Mastercard (production checklist)

The adapter is a real HTTPS client for Mastercard's REST API; the simulator
(`internal/modules/provider/mpgstest`, `cmd/mpgs-sim`) is never compiled into
the `paygate` binary or the Docker image.

1. **Credentials from the acquiring bank:** the gateway host (e.g.
   `https://ap-gateway.mastercard.com`, `https://eu-gateway.mastercard.com`
   or the bank's own MPGS host; never just `https://mastercard.com`), a TEST
   and a live merchant id (format is bank-specific), an API password for each,
   the API version and a webhook secret.
2. **Configure both, side by side:** live payments use `mpgs`, test mode keeps
   using the TEST merchant:
   ```
   PROVIDER_ROUTES=card=mpgs                 # live
   PROVIDER_TEST_ROUTES=card=mpgs_test       # test mode (keep it)
   MPGS_BASE_URL=https://<host from the bank>
   MPGS_MERCHANT_ID=...    MPGS_API_PASSWORD=...    MPGS_NOTIFICATION_SECRET=...
   MPGS_TEST_BASE_URL=https://<test host from the bank>
   MPGS_TEST_MERCHANT_ID=TEST...   MPGS_TEST_API_PASSWORD=...
   MPGS_CURRENCIES=USD     # only what the bank settles for this merchant account
   ```
3. **What paygate enforces:** the live merchant must use `https` to a public
   host (plain http is accepted only for the local simulator as the TEST
   merchant); TLS 1.2 or newer; no redirects followed; Basic auth
   `merchant.<id>` sent only to that host.
4. **Health check at start-up**, for each merchant:
   `GET /api/rest/version/{v}/information` (status must be `OPERATING`) and
   `GET .../merchant/{id}/paymentOptionsInquiry` (credentials accepted).
   Rejected credentials **stop start-up** with a clear message; an
   unreachable gateway is logged as `ALERT: Mastercard Gateway check failed`
   and start-up continues (the circuit breaker then reports card as
   unavailable until Mastercard answers). Alert on that log line, on
   `provider_unavailable` errors and on `GET /admin/providers` showing card
   unhealthy.
5. **PCI DSS:** Hosted Checkout keeps card data on Mastercard's page, so
   paygate never sees, logs or stores a card number, CVV or expiry
   (self-assessment SAQ A). 3-D Secure is run by Mastercard and the issuer.
6. **Before real customers:** pass the bank's UAT on the TEST merchant
   (approved, declined, insufficient funds, 3-D Secure failure, refund,
   webhook), have the bank whitelist your return/webhook domain (https),
   then do a penny test: a real $1.00 payment and refund on the live
   merchant, and confirm it on the bank settlement report.

### Try card payments locally (Mastercard simulator)

No bank account needed: `cmd/mpgs-sim` simulates the Mastercard Gateway,
including its hosted card page and 3-D Secure.

```bash
go run ./cmd/mpgs-sim        # http://127.0.0.1:9100, prints the test cards
# in .env (test mode only):
PROVIDER_TEST_ROUTES=card=mpgs_test
MPGS_TEST_BASE_URL=http://127.0.0.1:9100
MPGS_TEST_MERCHANT_ID=TESTPAYGATE
MPGS_TEST_API_PASSWORD=sim-password
MPGS_TEST_CURRENCIES=USD,ETB
```

Create a test payment with `"payment_method": "card"` and open its
`checkout_url`. Cards: `5123 4500 0000 0008` (Mastercard), `4508 7500 1574 1019`
(Visa), any 3-digit CVV, 3-D Secure code `123456`. The expiry decides the
bank's answer:

| Expiry | Result | Payment |
|---|---|---|
| `01/39` (or any other future date) | approved | `succeeded` |
| `02/39` | insufficient funds | `failed`, `failure_reason: insufficient_funds` |
| `05/39` | declined | `failed`, `declined` |
| `04/27` | bank says expired | `failed`, `expired_card` |
| `08/28` | bank timeout | `failed`, `timed_out` |
| wrong 3-D Secure code | authentication failed | `failed`, `authentication_failed` |

An invalid number (wrong check digit), a past expiry or a bad CVV is refused
on the card page itself, before the bank is asked. After a decline the
customer stays on the card page and may try another card; if they pay, the
payment becomes `succeeded` even if it was already marked failed (the
captured money always wins).
