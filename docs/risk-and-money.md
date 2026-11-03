# Risk, reserves, payouts, disputes and monitoring

[← back to README](../README.md)

## Merchant settings

Staff set them per merchant: `PATCH /admin/merchants/{id}` `{"settings": {...}}`
(the whole object is replaced).

```json
{
  "risk_level": "normal",
  "limits": {"ETB": {"max_amount": 5000000, "daily_amount": 50000000, "daily_count": 2000}},
  "reserve_bps": 500, "reserve_days": 14,
  "payout": {"schedule": "weekly", "weekday": 1, "currencies": ["ETB"], "min_amount": 100000,
             "bank": {"bank_name": "Commercial Bank of Ethiopia", "account_number": "1000123456789", "account_name": "Abebe Coffee PLC"}}
}
```

## Hard limits (payment module)

| Limit | Checked | Result |
|---|---|---|
| `max_amount` | each payment | `422 limit_exceeded` |
| `daily_amount`, `daily_count` | last 24 hours, payments that can take or took money | `422 limit_exceeded` |

Checked and stored **under a per-merchant lock**: ten payments at once against
`daily_count: 5` give exactly five. **Fails closed**: if the check cannot run,
no payment is created.

## Fraud rules (risk module)

Every payment is scored before it reaches a provider, from the customer data
the merchant sends (`customer.email`, `customer.phone`, `customer.ip`):

| Rule | Points |
|---|---|
| email, phone or IP on the blocklist | 100 |
| 5+ attempts by one customer in 10 min, or 20+ in a day | 100 |
| 10+ attempts from one IP in 10 min | 100 |
| 3+ declines of one customer in an hour (card testing) | 60 (5+: 100) |
| amount over 10x the merchant's 30-day average | 40 |
| merchant `risk_level: high` (thresholds also halved) | 20 |

**100 blocks**: the payment is created as `failed` (`failure_reason:
risk_declined: <rules>`, source `risk` in its history) and the API answers
`422 risk_declined`. Every decision is recorded:
`GET /admin/risk/decisions?action=block`. Blocklist:
`POST /admin/risk/blocklist {kind, value, reason}` (`risk:manage`).

If the risk module cannot answer, the payment continues (fail open for scoring
only; the hard limits still apply) and `ALERT: risk checks unavailable` is
logged.

## Rolling reserve

`reserve_bps` of the merchant's share of each payment (after the fee) is held
for `reserve_days`, as cover for refunds and chargebacks. It is a snapshot at
creation, like the fee.

```
payment 1,000.00, fee 1.5 %, reserve 10 %, 7 days
  provider clearing  +1,000.00
  platform fees         15.00
  merchant available   886.50
  merchant reserve      98.50   → released to available after 7 days (ledger worker)
```

`GET /v1/balance` shows `available`, `reserved`, `dispute_hold`,
`pending_payouts`. Only `available` can be paid out.

## Scheduled payouts

With `payout.schedule` `daily` or `weekly` (on `weekday`), the payout module
pays out the whole `available` balance (if at least `min_amount`) to the bank
account staff verified during KYC. Live money only, active KYC-approved
merchants only. **At most one scheduled payout per merchant, currency and
business day**: the idempotency key `schedule:<currency>:<date>` makes a
second one impossible, also with several replicas. Finance then sends the
transfer and marks it paid, as for any payout.

## Disputes (chargebacks)

```
staff: POST /admin/disputes {payment_id, amount, reason_code, provider_case_id}
   → needs_response   money held at once (available → dispute_hold); webhook dispute.opened
merchant: POST /v1/disputes/{id}/evidence  (text + up to 5 PDF/JPEG/PNG, 5 MB each)
          POST /v1/disputes/{id}/submit    → under_review
          POST /v1/disputes/{id}/accept    → lost
staff: POST /admin/disputes/{id}/close {outcome: won | lost, fee}
   won:  dispute_hold → available
   lost: dispute_hold → provider clearing (back to the cardholder); fee: available → platform fees
no evidence by evidence_due_by → lost automatically
```

* A payment can be disputed up to what is left of it (captured − refunded −
  disputed); concurrent disputes are serialised per payment.
* Each step is in the dispute's history and noted in the payment's history
  (source `dispute`). Webhooks: `dispute.opened`, `dispute.closed`.
* Merchant permissions `disputes:read`, `disputes:write`; staff `disputes:manage`.
  Existing merchant roles do not get them automatically: add them with
  `POST /admin/roles/{id}/permissions`.
* Evidence files are kept in the dispute database (bytea). For large volumes,
  move them to object storage like KYC documents.

## Monitoring

`GET /metrics` (Prometheus format; `METRICS_TOKEN` protects it):

| Metric | Alert when |
|---|---|
| `paygate_http_requests_total{route,status}`, `paygate_http_request_duration_seconds` | 5xx rate or p99 latency up |
| `paygate_payments_created_total`, `paygate_payments_final_total{status,reason,provider}` | success rate per provider drops |
| `paygate_payments_open{status="stuck"}` | > 0 for 15 min |
| `paygate_provider_requests_total{outcome}`, `paygate_provider_request_duration_seconds` | `unavailable` / `unknown` rising |
| `paygate_payment_risk_total{outcome}`, `paygate_risk_decisions_total` | `risk_unavailable` > 0, block rate spikes |
| `paygate_outbox_pending{module,stat}` | `oldest_seconds` > 60 |
| `paygate_webhook_deliveries{status}` | `failed` growing |
| `paygate_reconciliation_open_items{severity}` | `critical` > 0 |
| `paygate_disputes_open{status}` | — |
| `paygate_ledger_unbalanced{currency}` | **= 1: page someone** |

Routes are labelled by their pattern (`/v1/payments/{id}`), never the raw
path, so ids do not create new series.
