# Audit log (who did what)

[← back to README](../README.md)

Every action on the backend is recorded: by merchant team members, merchant
API keys, platform staff, customers on the checkout page and payment
providers calling back. Recording happens in one place (an HTTP middleware
in every service), so a new endpoint is recorded without extra code.

## What is recorded

| Recorded | Always | With `AUDIT_ALL_READS=true` |
|---|---|---|
| Changes (POST, PUT, PATCH, DELETE), including refused and failed ones | ✅ | ✅ |
| Every staff request under `/admin` (including reads) | ✅ | ✅ |
| Sensitive reads: KYC documents, dispute files, CSV exports, API keys, webhook endpoints, team, the audit log itself | ✅ | ✅ |
| Sign-ins (success and failure, with the email tried), verification, password reset | ✅ | ✅ |
| Rejected requests: bad or expired tokens, missing permissions, rate limits | ✅ | ✅ |
| Other reads (payment lists, balances ...) | | ✅ |

Each entry: time, who (`actor_kind`: user, api_key, staff, customer,
provider, anonymous; `actor_id`; a label such as the email on sign-in),
which business (`merchant_id`), the action (`payments.create`,
`admin.payouts.paid`, `auth.login` ... named from the route), the record
(`resource_type`, `resource_id`, including the id of a record just created),
the result (`success`, `denied`, `rejected`, `rate_limited`, `error`, the
HTTP status and the API error code), IP address, device (user agent),
correlation id and duration.

**Never recorded:** request bodies, query strings, headers, passwords,
verification and authenticator codes, tokens, API keys, card data. One-time
file links are not recorded as record ids.

## Who can read it

| Who | Endpoint | Sees |
|---|---|---|
| Merchant team (permission `audit:read`) | `GET /v1/audit-logs`, dashboard → **Activity log** | **only its own business**: the merchant comes from the signed token; a `merchant_id` in the query is ignored. Includes failed sign-ins to its team's accounts. |
| Platform staff (permission `audit:read_all`) | `GET /admin/audit-logs` | every action, any merchant; optional `merchant_id` filter |
| Platform staff | `GET /admin/audit-logs/verify` | recomputes the hash chain |

API keys cannot read the log. Filters: `actor_id`, `actor_kind`, `action`
(prefix), `resource_id`, `outcome`, `from`, `to`, `limit`, `starting_after`.

## Tamper protection

* **Append-only:** the database refuses `UPDATE`, `DELETE` and `TRUNCATE` on
  `audit_logs`.
* **Hash chain:** every entry stores the SHA-256 of its own content and of
  the previous entry. If someone with database superuser rights changes or
  removes an entry, `GET /admin/audit-logs/verify` reports the first broken
  entry. Run it daily (and keep the last `last_hash` it returns somewhere
  else, e.g. in a ticket, so even a rewrite of the whole chain is visible).
* **Run the application with an ordinary database role**, not a superuser:
  only a superuser can disable triggers.

## Delivery

Recording never slows a request: each service queues entries (10,000) and
sends them to the audit service in the background, with retries. If the
audit service cannot be reached, the entry is written to the service's log
as `audit_entry_unsent` (with the full entry) and the alert
`AuditEntriesUnsent` fires. On a clean shutdown the queue is emptied first.

## Retention

Entries are kept forever by default. Payment regulations usually require
several years; archive old months to cold storage (export, verify the chain,
then move the partition) rather than deleting rows.
