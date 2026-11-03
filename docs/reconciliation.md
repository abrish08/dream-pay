# Payment history, status checks and reconciliation

[← back to README](../README.md)

Three mechanisms keep every payment provably correct:

| | What | When |
|---|---|---|
| **History** | an append-only record of every status change: who, why, what the provider said | on every change, in the same transaction |
| **Status checks** | open payments are verified with the provider | 1, 3, 8, 18, 38 and 68 minutes after the checkout opened |
| **Reconciliation** | our payments vs the provider vs the ledger vs the settlement file | daily, automatically; on demand; on each settlement upload |

## Payment history

`GET /v1/payments/{id}/events` (merchant) or `GET /admin/payments/{id}/events` (staff, `payments:read_all`):

```json
{"data": [
  {"from_status": "",           "to_status": "pending",    "source": "merchant_api"},
  {"from_status": "pending",    "to_status": "processing", "source": "merchant_api", "reason": "checkout opened", "provider": "telebirr"},
  {"from_status": "processing", "to_status": "succeeded",  "source": "status_check", "details": {"provider_status": "succeeded", "amount": 150000, "currency": "ETB"}},
  {"from_status": "succeeded",  "to_status": "partially_refunded", "source": "refund", "reason": "refund 9c1…"}
]}
```

| `source` | Meaning |
|---|---|
| `merchant_api` | the merchant's server created or started it |
| `customer` | the customer chose a method on the hosted page |
| `provider` | the provider's notification (always re-verified with the provider) |
| `status_check` | our scheduled check with the provider found the result |
| `expiry` | the customer's time ran out (after asking the provider) |
| `refund` | money was paid back |
| `reconciliation` | the daily check found and repaired a difference |

* Written in the **same database transaction** as the change, so the history
  can never disagree with the payment.
* **Append-only, enforced by the database**: a trigger refuses `UPDATE` and
  `DELETE` on `payment_events`. Corrections are new entries, never edits.
* Existing payments got one "history started" entry when the table was added.

## When the provider does not answer

The rule: **a timeout is not a failure.** The customer may already have the
telebirr PIN prompt on their phone.

| What happened | Result |
|---|---|
| The provider refused before accepting (connection refused, circuit open) | next provider in the route; if none, the payment **fails** |
| A business refusal (invalid phone, unsupported currency) | the payment **fails**; no failover (it would fail elsewhere too) |
| **No answer** (timeout, connection lost after sending) | the payment stays **`processing`**, is **not** sent to another provider, and is checked with the provider until the result is known |

* The provider attempt is recorded **before** the provider is called, so a
  crash leaves a record to verify instead of a lost payment.
* A retried request never sends a second checkout for the same payment.
* Sandbox: amounts ending in **…9.99** simulate "no answer"; **…9.98**
  simulate "connection refused" (e.g. `20999` = ETB 209.99).

## Status checks

The payment module's `StatusCheckWorker` claims due payments with
`FOR UPDATE SKIP LOCKED` (safe with many replicas) and asks the provider.
A lost callback is found in about a minute instead of at expiry. After the
schedule, the expiry check asks the provider one last time before expiring.

## Reconciliation

### The daily run (automatic)

Every day, one hour after midnight in `RECONCILIATION_TIMEZONE` (default
`Africa/Addis_Ababa`), the previous business day is checked. One run per day,
even with many replicas. Each payment of the day is checked three ways:

| Check | Finds | Severity |
|---|---|---|
| Provider API | customer charged but payment `failed`/`expired` (**repaired automatically**) | critical |
| | payment `succeeded` but the provider has no capture | critical |
| | different amount or currency | critical |
| | provider has no record of a captured payment | high |
| Ledger | captured but never booked (merchant not credited) | critical |
| | booked with a different amount | critical |
| Age | still `processing` after 1 hour | high |

### Settlement files

Upload each provider's settlement report as CSV:

```bash
curl -X POST "$API/admin/reconciliation/settlements?provider=mpgs&date=2026-11-02&deposit_amount=96902.50&deposit_currency=ETB" \
  -H "Authorization: Bearer $STAFF_TOKEN" -H "Content-Type: text/csv" --data-binary @mpgs-2026-11-02.csv
```

```csv
reference,type,amount,fee,currency,settled_at
7b9e2c1a-3f4d-4e8a-9b6c-1d2e3f4a5b6c,payment,1500.00,22.50,ETB,2026-11-02
```

| Finds | Severity |
|---|---|
| settled with a different amount | critical |
| settled but we did not capture it (repaired if the provider confirms) | critical |
| bank deposit differs from the report's net | critical |
| the same payment settled twice | high |
| captured but not in any report yet | medium (closed automatically when a later report contains it) |

The whole file is refused if any line is bad (every bad line is listed), and
the same file cannot be counted twice.

### The work queue

`GET /admin/reconciliation/items?status=open&severity=critical` lists what
needs a person. A difference found again the next day is not reported twice.
Finance closes an item with `POST /admin/reconciliation/items/{id}/resolve`
`{"note": "what was found and done"}` (permission `reconciliation:resolve`,
finance only: those who run checks are not those who sign them off).

Run summaries carry totals per currency (captured, settled, fees, refunds,
net). A run with critical differences is logged at ERROR with `alert=true`:
alert on that line.
