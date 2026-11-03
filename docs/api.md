# API summary

[← back to README](../README.md)


**Integrators: every endpoint is documented in the app**: `/swagger`
(Swagger UI, try requests), `/docs` (reference) and `/openapi.yaml` (for SDK
generators). `TestAPIDocsCoverEveryRoute` fails the build if a route is
missing from the spec.

| Method | Path | Who / permission | Service |
|---|---|---|---|
| POST | `/auth/signup` (merchant self-signup → pending, test mode at once) | none | auth |
| POST | `/auth/login`, `/auth/refresh`, `/auth/logout` | none | auth |
| GET | `/v1/merchant` (own account, status, fee) | any merchant caller | merchant |
| GET, PATCH | `/admin/merchants?status=pending`, PATCH `/admin/merchants/{id}` `{fee_bps}` | staff, `merchants:read` / `merchants:manage` | merchant |
| POST, GET | `/v1/payouts[/{id}]` (POST: logged-in user only) | `payouts:write` / `payouts:read` | payout |
| GET, POST | `/admin/payouts?status=requested`, POST `/admin/payouts/{id}/paid` / `/failed` | staff, `payouts:manage` | payout |
| GET | `/v1/refunds?payment_id=` | `payments:read` | refund |
| POST | `/v1/webhook-endpoints/{id}/rotate-secret`, `/v1/webhook-deliveries/{id}/retry` | `webhooks:write` | webhook |
| GET | `/auth/me`; POST `/auth/password` | any logged-in caller | auth |
| GET | `/.well-known/jwks.json` | none | auth |
| POST, GET, DELETE | `/v1/api-keys[/{id}]`, POST `/v1/api-keys/{id}/roll` | merchant user, `apikeys:manage` | auth |
| POST, GET, DELETE | `/v1/team/members[/{id}]` | merchant user, `team:manage` | auth |
| POST, DELETE | `/v1/team/members/{id}/roles[/{roleId}]` | `team:manage` | auth |
| GET | `/v1/permissions`, `/v1/roles[/{id}]`, `/v1/audit-log` | merchant user, `roles:read` | auth |
| POST, PATCH, DELETE | `/v1/roles[/{id}]`, POST/DELETE `/v1/roles/{id}/permissions[/{code}]` | merchant user, `roles:manage` | auth |
| POST, GET, DELETE | `/admin/users[/{id}]`, POST/DELETE `/admin/users/{id}/roles[/{roleId}]` | staff, `users:manage` | auth |
| same as `/v1` | `/admin/permissions`, `/admin/roles…`, `/admin/audit-log` (`?audience=merchant` for templates) | staff, `roles:read` / `roles:manage` | auth |
| GET, DELETE | `/admin/merchants/{id}/api-keys[/{keyId}]` | staff, `merchants:read` / `merchants:manage` | auth |
| POST, GET | `/admin/merchants`, `/admin/merchants/{id}`, `.../status` | staff, `merchants:manage` / `merchants:read` | merchant |
| POST | `/v1/payments` (+ `Idempotency-Key`) | API key, `payments:write` | payment |
| GET | `/v1/payments`, `/v1/payments/{id}` | `payments:read` | payment |
| POST | `/v1/refunds` (+ `Idempotency-Key`) | `refunds:write` | refund |
| GET | `/v1/refunds/{id}` | `payments:read` | refund |
| GET | `/v1/balance?currency=ETB[&environment=test]` | `balance:read` | ledger |
| GET | `/admin/ledger/trial-balance` | staff, `ledger:read` | ledger |
| POST, GET, DELETE | `/v1/webhook-endpoints[/{id}]`, GET `/v1/webhook-deliveries` | `webhooks:write` | webhook |
| POST, GET | `/admin/reconciliation/runs[/{id}]` | staff, `reconciliation:run` | reconciliation |
| GET, POST | `/providers/{name}/callback`, `/providers/mock/checkout/{txRef}` | verified per provider | provider |
| GET | `/healthz` | none | every process |

Errors always look like `{"error":{"kind":"...","code":"...","message":"..."}}`.
Amounts are integers in minor units (`150000` = 1,500.00 ETB). `sk_test_`
keys can only use the `mock` provider; test and live money use separate
ledger accounts. Webhooks carry `Paygate-Signature: t=<unix>,v1=<hex>` where
`v1 = HMAC-SHA256(secret, "<unix>.<raw body>")`.
