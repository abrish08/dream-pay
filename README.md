# paygate: a payment gateway as a modular monolith with a central bus (Go)

Eight independent modules that **never depend on each other**. They share
only a set of message definitions (`internal/contracts`) and talk through
**one central bus** (`internal/platform/bus`). The same binary runs as:

* **one monolith**: `MODULES=all BUS=memory`
* **any split**: `MODULES=payment BUS=nats`, one module per service if you want, each on its own database

No code changes and no service URLs: the bus finds whichever process handles a request.

```
           ┌──────────────────────────── internal/contracts ────────────────────────────┐
           │  merchants    auth     providers   payments   refunds   (requests + events) │
           └──────▲─────────▲──────────▲──────────▲──────────▲─────────────────────────────┘
                  │ imports only contracts + platform, never another module
 ┌────────┐ ┌──────┴─┐ ┌─────┴──┐ ┌──────┴──┐ ┌─────┴──┐ ┌─────┴──┐ ┌──────┐ ┌───────┐ ┌──────────────┐
 │merchant│ │  auth  │ │provider│ │ payment │ │ refund │ │ ledger │ │webhook│ │ reconciliation│
 └───┬────┘ └───┬────┘ └───┬────┘ └────┬────┘ └───┬────┘ └───┬────┘ └──┬───┘ └──────┬───────┘
     └──────────┴──────────┴───────────┴── central bus ──────┴─────────┴────────────┘
                     memory (one process)   |   NATS (many processes)
```

## Quick start

Needs Go 1.24+ and PostgreSQL. Docker and `make` are optional.

```bash
./scripts/run.sh env      # creates .env with freshly generated secrets, prints the first admin's password
# edit .env: DATABASE_URL=postgres://<user>:<password>@localhost:5432/gateway?sslmode=disable
./scripts/run.sh db       # creates the "gateway" database (tables are created on start)
./scripts/run.sh          # starts on http://localhost:8080

./scripts/run.sh demo     # in another terminal: login, onboarding, API key, payment, refund, checks
```

`.env.example` holds placeholders only (`CHANGE_ME`); the app will not start
until they are replaced. `.env` is git-ignored: never commit it.

With `make` installed: `make env`, `make db`, `make run`, `make demo`.

Everything in Docker:

```bash
./scripts/run.sh env                          # once: compose reads the secrets from .env
docker compose --profile mono  up --build     # monolith
docker compose --profile micro up --build     # 9 services + 9 databases + NATS + edge gateway
```

Both listen on http://localhost:8080 and `scripts/demo.sh` runs unchanged against either.

## The three rules

1. **A module imports only `internal/contracts/...` and `internal/platform/...`.**
   Never another module, never `internal/app`. `scripts/check-boundaries.sh`
   enforces this in CI.
2. **Every cross-module message is defined in `internal/contracts`.** One
   sub-package per owning domain. The owner handles its requests and publishes
   its events; anyone may send those requests or subscribe to those events.
   Contracts hold only DTOs, requests, events, error codes and constants.
3. **A module owns its data.** Its own schema (or database), migrations, outbox
   and processed-events table. No cross-module joins or foreign keys.

## How modules communicate

### Requests: when you need an answer now

A contract declares a request and its response type:

```go
// internal/contracts/merchants/merchants.go
type GetMerchant struct {
    bus.Returns[Merchant]  // the response type
    ID string `json:"id"`
}
func (GetMerchant) Route() string { return "merchants.get" }
```

The owning module answers it:

```go
// internal/modules/merchant/internal/transport/transport.go
bus.Handle(b, func(ctx context.Context, q merchants.GetMerchant) (merchants.Merchant, error) {
    return svc.GetMerchant(ctx, q.ID)
})
```

Any other module asks, without knowing where the merchant module runs:

```go
// internal/modules/payment/internal/service/service.go
m, err := bus.Ask(ctx, s.bus, merchants.GetMerchant{ID: in.MerchantID}) // m is merchants.Merchant
```

The response type is inferred and checked at compile time. Errors keep their
code across the bus, so `errors.Is(err, merchants.ErrNotFound)` works
everywhere. The correlation id and the caller's **signed access token** travel with
every request.

### Events: when you announce that something happened

```go
// publish (payment module), in the SAME DB transaction as the state change
s.events.Publish(ctx, tx, payments.PaymentSucceeded{PaymentID: p.ID, ...})

// subscribe (ledger module)
bus.On(b, "ledger.on_payment_succeeded",
    func(ctx context.Context, env bus.Envelope, e payments.PaymentSucceeded) error { ... })
```

* **Outbox**: events are written to the publisher's own `outbox` table with the
  business change, so none are lost or sent for rolled-back work.
* **Relay**: each module moves its outbox to the bus (`FOR UPDATE SKIP LOCKED`,
  safe with replicas, exponential backoff).
* **HandleOnce**: consumers record `(event_id, consumer)` with their work, so
  at-least-once delivery never applies an event twice.

### Bus transports

| | `BUS=memory` | `BUS=nats` |
|---|---|---|
| Requests | direct call (still JSON-encoded) | NATS request-reply, subject `paygate.req.<route>` |
| Load balancing | n/a | queue group per route: replicas share requests automatically |
| Discovery | n/a | none needed: callers address routes, not hosts |
| Events | delivered synchronously | JetStream, one durable consumer per subscriber |
| Local shortcut | always | requests for a route handled in the same process skip the network |

Messages are JSON-encoded even in memory, so a module behaves exactly the same
whether its peer is in-process or in another service.

## Modules and their messages

| Module | Handles (requests) | Asks | Publishes | Subscribes |
|---|---|---|---|---|
| **merchant** | `merchants.GetMerchant`, `CreateMerchant` | – | `MerchantUpdated` | – |
| **auth** | `auth.ExchangeAPIKey`, `RegisterPermissions` | `GetMerchant`, `CreateMerchant` (signup) | `AccessChanged` | `MerchantUpdated` |
| **provider** | `providers.InitiateCheckout`, `VerifyTransaction`, `Refund` | – | `providers.TransactionUpdated` | – |
| **payment** | `payments.GetPayment`, `ListPayments` | `GetMerchant`, `InitiateCheckout`, `VerifyTransaction` (expiry) | `PaymentSucceeded`, `PaymentFailed`, `PaymentExpired` | `TransactionUpdated`, `RefundSucceeded` |
| **refund** | – | `GetPayment`, `providers.Refund` | `RefundSucceeded`, `RefundFailed` | – |
| **ledger** | `ledger.ReserveFunds` | – | – | `PaymentSucceeded`, `RefundSucceeded`, `PayoutPaid`, `PayoutFailed` |
| **payout** | – | `GetMerchant`, `ReserveFunds` | `PayoutPaid`, `PayoutFailed` | – |
| **webhook** | – | – | – | payment.\*, refund.\*, payout.\* |
| **reconciliation** | – | `ListPayments`, `VerifyTransaction` | – | – |

Every service authenticates its own HTTP requests (see *Authentication and RBAC*).

## Payment flow

```
1  POST /v1/payments (API key, Idempotency-Key)
     payment ──Ask──► merchants.GetMerchant         (active? fee?)
     payment ──Ask──► providers.InitiateCheckout    (checkout_url)            status: processing
2  customer pays on the provider page
3  provider callback → provider verifies with the provider's API (never trusts the callback)
     provider ──event──► providers.TransactionUpdated
4  payment checks amount/currency → succeeded
     payment ──event──► payments.PaymentSucceeded
5  ledger:  Dr provider clearing 1500.00 / Cr merchant 1477.50 / Cr fees 22.50
   webhook: signed delivery to each merchant endpoint (retried with backoff)
```

Refunds: refund asks `payments.GetPayment`, locks the payment, checks what is
still refundable from its **own** refunds table, asks `providers.Refund`
(outside any DB transaction) and publishes `refunds.RefundSucceeded`. Payment
updates its status, ledger debits the merchant, webhook notifies. If the
provider is unreachable the refund stays `pending` and a worker retries it;
it is never marked failed on a timeout.

## From monolith to microservices

Nothing in `internal/modules` or `internal/contracts` changes. Only deployment does.

| Step | Change |
|---|---|
| 1 | Switch `BUS=memory` → `BUS=nats` while still `MODULES=all` (proves everything works through a broker) |
| 2 | Run a second deployment with `MODULES=webhook` (for example) and remove `webhook` from the first |
| 3 | Route that module's HTTP paths to the new deployment at your API gateway (see `cmd/edge` for the table) |
| 4 | Move the module's schema to its own database (`pg_dump -n webhook`, or logical replication) and set `DATABASE_URL` / `DB_URL_WEBHOOK` |
| 5 | Scale: run more replicas; NATS queue groups spread requests, `SKIP LOCKED` keeps relays and workers safe |

Every step is reversible by changing configuration back. A module whose
handlers no process is running returns `no_handler` (HTTP 502) instead of
hanging.

The `micro` compose profile is the end state: **every module is its own
service on its own database**, with two payment replicas behind a NATS queue group.

## Authentication and RBAC

### Who can call what

| Caller | Credential | Reaches | Gets permissions from |
|---|---|---|---|
| Platform staff | login → access token (JWT) | `/admin/*` | platform role |
| Merchant team member | login → access token (JWT) | `/v1/*` (own merchant only) | merchant role |
| Merchant server | API key `sk_live_…` / `sk_test_…` | `/v1/*` (own merchant only) | the key's scopes |

### Signing in

| Way | Endpoint | Notes |
|---|---|---|
| Sign up | `POST /auth/signup` → 6-digit code → `POST /auth/verify` | no token until the code is confirmed |
| Email + password | `POST /auth/login` | argon2id; 5 failures lock 15 min; unverified → `403 verification_required` + new code |
| Google / Apple | `POST /auth/oauth/google`, `/auth/oauth/apple` with the app's ID token | provider-verified email; signature, issuer, audience, expiry, nonce checked |
| Authenticator step | `POST /auth/login/mfa` `{mfa_token, code}` | only when the user enabled an authenticator app |
| Refresh | `POST /auth/refresh` | refresh token rotates; re-use of an old one signs the user out everywhere |
| Forgot password | `POST /auth/password/forgot` → email → `POST /auth/password/reset` | always 202; single-use 256-bit token, 30 min; 3 emails/hour |
| Change password | `POST /auth/password` `{current_password, new_password, totp_code}` | **requires an authenticator code** |

Every completed sign-in returns an **access token** (JWT, 5 min, carries roles
and permissions) and a **refresh token** (7 days, stored hashed, rotated).

#### Account verification (6-digit code)

* Self-signup and team members invited by a merchant must verify before
  getting any token. The code goes **by email** (any SMTP provider) or, with
  `verification_channel: "sms"` and a phone number, **by SMS through send.et**.
  send.et is an SMS gateway; it does not send email.
* Codes: random 6 digits, stored as HMAC(pepper, user, code), valid 15 min,
  single use, 5 wrong tries kill the code, a new code replaces the old one,
  at most 1 send per minute and 5 per hour. `POST /auth/verify/resend` always
  answers 202 (no account enumeration).
* **Admins bypass it**: platform staff, and accounts created by platform staff
  (an administrator vouched for them), never need a code.
* A verified email (code, Google/Apple, or completed password reset) is what
  allows linking Google/Apple to an existing account.

#### Authenticator app (Microsoft Authenticator, TOTP)

1. `POST /auth/mfa/totp/setup` → `secret` + `otpauth_url` (show it as a QR code).
2. Scan with Microsoft Authenticator (Google Authenticator, Authy... work too).
3. `POST /auth/mfa/totp/confirm {code}` → enabled; **10 recovery codes** shown once.

Then: changing the password needs `totp_code` from the app (a recovery code is
not accepted for that); login asks for the app code as a second step
(`mfa_required`), where a recovery code also works once. Disabling needs the
password and a code. Secrets are encrypted with `DATA_ENCRYPTION_KEY`; a code
is accepted only once (no replay); wrong codes count toward the account lockout;
enabling, disabling and using recovery codes trigger an email notice.

#### One account per email

Signing up with a password for an email that already has an account (any
method) is `409 email_taken`. Signing in with Google/Apple when an account
already uses that (provider-verified) email **links** to it: never a second
account. If that account's email was never verified, its password is removed
and its sessions end when the verified owner links (prevents account
pre-hijacking).

#### Other protections

* Security headers on every response: `nosniff`, no framing, no referrer,
  strict Content-Security-Policy (`default-src 'none'` for the API), HSTS when
  `HSTS=true`.
* Per-IP rate limits on signup, login, verify, MFA and password endpoints; per
  key/user limits elsewhere.
* Every security event (signup, verification, password change/reset, MFA on/off,
  role changes) goes to the audit log; sensitive ones also email the user.
* Password change, password reset, role changes and disabling a user make the
  user's existing access tokens stale everywhere within seconds.

**Default admin.** On first start, `BOOTSTRAP_ADMIN_EMAIL` /
`BOOTSTRAP_ADMIN_PASSWORD` create a `super_admin`: a locked role holding every
platform permission, including permissions of modules added later. It is
verified by definition. Turn on its authenticator right after the first login.

### Dynamic RBAC: user → many roles → many permissions

```
permission  defined in CODE by the service that checks it, registered with
            auth at start-up (contracts/<service>/…Permissions)
role        stored by auth, edited at runtime: add / remove permissions
user        has many roles; token permissions = union of all its roles
```

* **Every endpoint checks one permission** (`httpx.Require(payments.PermWrite)`);
  `/v1` also requires a merchant caller and `/admin` a platform caller.
* **Registration.** On start each service calls `auth.RegisterPermissions`
  (retried in the background if auth is not up yet). Auth upserts the codes,
  marks codes a service stopped sending as *deprecated* (never deleted), and
  gives a **new** permission to the built-in roles listed in its
  `DefaultRoles`. Later edits to those roles are never overwritten.
* **Built-in roles** (created once): platform `super_admin` (every platform
  permission, locked), `operations`, `finance`, `support`; merchant templates
  `owner` (every merchant permission, locked), `admin`, `developer`,
  `analyst`, `viewer`. Platform staff can edit the non-locked ones; merchants
  create their own **custom roles**.
* **Tokens.** At login/refresh auth merges the user's roles into the signed
  token (`roles`, `permissions`). Services check the token only, with no lookup.
  Access tokens live 5 minutes. When roles or permissions change, auth
  publishes `auth.access_changed.v1`; **every** process (broadcast
  subscription) then rejects older tokens with `401 token_stale`, and the
  client calls `/auth/refresh`.
* **Anti-escalation.** You can only grant permissions you hold; `owner` /
  `super_admin` can only be granted or removed by a holder; nobody changes
  their own roles; the last owner / super admin cannot be removed; merchant
  roles hold merchant permissions only. Every change goes to `audit_log`.

### How a request is authenticated (in every service, no central gateway logic)

```
dashboard user:  POST /auth/login ─► access token (15 min, Ed25519-signed) + refresh token (7 days, rotated)
                 each request: service verifies the token signature locally   → no network call

merchant server: Authorization: Bearer sk_live_...
                 service ──bus──► auth.ExchangeAPIKey ─► 5-min signed token, cached AUTH_KEY_CACHE_TTL (30s)
                 → at most one auth call per key per 30s per service instance

service ──bus──► service: the signed token travels with the request and is
                 verified again by the receiver. A service cannot claim to be
                 a merchant it is not; unsigned identities are dropped.
```

* Only the **auth** service has `AUTH_SIGNING_KEY` (private). All services get
  `AUTH_PUBLIC_KEYS`, which may list several keys for zero-downtime rotation.
  Public keys are also served at `/.well-known/jwks.json`.
* Merchant status reaches auth through `merchant.updated` events, so login and
  key checks don't call the merchant service; suspending a merchant blocks its
  keys and users within seconds.
* Passwords: **argon2id** (OWASP parameters), constant-time checks, a dummy
  check for unknown emails (no account enumeration), lockout after 5 failures
  for 15 minutes. Changing a password ends all sessions.
* Refresh tokens are stored as SHA-256, rotated on every use; re-using an old
  one revokes all the user's sessions.

### API key management

Only a **logged-in merchant user** with `apikeys:manage` can create, list,
roll or revoke keys. An API key can never manage keys or users, so a leaked key
cannot create more keys.

| Decision | Choice | Why |
|---|---|---|
| Generate | 256 bits from `crypto/rand` | cannot be guessed or brute-forced |
| Format | `sk_live_` + 43 base62 + 6-char CRC32 checksum | secret scanners spot leaks; test/live visible; typos rejected without a DB lookup; double-click selectable |
| Store | **HMAC-SHA256(pepper, key)** only, plus `prefix` and `last4` for display | the raw key is never stored |
| Not encryption | — | keys never need to be read back; anything decryptable can leak |
| Not bcrypt/argon2 | — | those protect *low-entropy* passwords; for 256-bit keys they only add ~50 ms to every API call |
| Pepper | in a secret manager, never in the DB | a database leak alone is useless |
| Pepper rotation | `API_KEY_PEPPER_PREVIOUS` + `API_KEY_PEPPER_VERSION` | old keys keep working and are re-hashed on first use |
| Show once | the response is `Cache-Control: no-store` | cannot be recovered; lost key → roll it |
| Least privilege | scopes ⊆ `APIKeyScopes` ∩ creator's permissions | a developer cannot mint a refund key |
| Lifetime | optional `expires_in_days`; revoke; **roll** with a grace period (default 24 h) | rotate without downtime |
| Limits / audit | 25 active keys per merchant; `created_by`, `last_used_at`; create/roll/revoke logged | |

Platform support can list and revoke a merchant's keys (e.g. a reported leak)
but never create them.

## API summary

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

## Configuration (auth)

| Variable | Where | Purpose |
|---|---|---|
| `AUTH_SIGNING_KEY` | auth service only | Ed25519 private seed (`make keygen`) |
| `AUTH_PUBLIC_KEYS` | every service | comma-separated public keys (old + new while rotating) |
| `AUTH_KEY_CACHE_TTL` | every service | API key token cache, default `30s`, `0` = off |
| `API_KEY_PEPPER`, `API_KEY_PEPPER_VERSION`, `API_KEY_PEPPER_PREVIOUS` | auth only | key hashing pepper and its rotation |
| `BOOTSTRAP_ADMIN_EMAIL`, `BOOTSTRAP_ADMIN_PASSWORD` | auth only | first `super_admin`, created once if none exists |

All secrets belong in a secret manager (e.g. Azure Key Vault via Managed
Identity), never in Git or the database.

## Security

* Webhook secrets: AES-256-GCM encrypted (they must be read back to sign).
* Provider callbacks are always re-verified with the provider's API.
* In production, secure NATS with TLS and per-service credentials (each
  service may only answer its own routes and publish its own events), and
  keep it on a private network.
* Webhook delivery never follows redirects; `http://` URLs are rejected unless
  `WEBHOOK_ALLOW_INSECURE=true` (development only).

## Tests

```bash
go test -race ./...                                   # unit tests
TEST_DATABASE_URL=postgres://postgres:postgres@localhost:5432/paygate_test?sslmode=disable \
TEST_NATS_URL=nats://localhost:4222 \
  go test -race -count=1 ./internal/app/ ./internal/platform/bus/   # full flow + cross-process bus
./scripts/check-boundaries.sh                         # no module may import another
```

The full-flow test covers onboarding, idempotency (replay and conflict),
checkout, event propagation, balances, partial and over-refunds, a
provider-declined refund, signature checks on every webhook, the trial
balance, reconciliation, scopes and merchant suspension. The bus test runs
two separate NATS clients to prove requests, typed errors and events cross
process boundaries.

## Database migrations

Every module migrates **its own schema** on start, in one transaction behind
an advisory lock (safe with many replicas):

1. `migrations/*.sql`: applied once each (used for outbox tables, partial
   indexes, data fixes, renames).
2. **GORM AutoMigrate** of the models a module registers (`base.Models(...)`):
   creates missing tables, columns and indexes. It is **additive only**: a
   guard refuses any `DROP` AutoMigrate would issue, so existing data is never
   deleted. To change the schema, edit the struct; to drop or rename, write a
   SQL file on purpose.

The auth module uses models only (`internal/modules/auth/internal/store/models.go`).
`TestAutoMigrateUpgradesWithoutLoss` upgrades a database created by the old
SQL file and checks that every row survives and a second start changes nothing.
Other modules still use SQL files and can move to models the same way.

## Layout

```
cmd/paygate/            the service binary (MODULES decides what runs inside)
cmd/edge/               dev API gateway: routes HTTP paths to the owning service
cmd/loadtest/           load generator (goroutines): throughput and latency
internal/app/           composition root: bus + enabled modules (+ e2e test)
internal/contracts/     THE shared language: requests, events, DTOs, error codes
  merchants/ auth/ providers/ payments/ refunds/ ledger/ webhooks/ reconciliation/
                        each owner declares its permissions here
internal/platform/      technical kernel, no business logic
  bus/                  central bus: typed Ask/Handle/On, memory + NATS transports
  events/               per-module outbox, relay, HandleOnce
  module/               the Module interface every module implements
  authn/                Ed25519 access tokens: Signer (auth only), Verifier (everyone)
  apperror config database httpx money reqctx secrets
internal/modules/<name>/
  internal/domain|store|service|transport/   private
  migrations/           this module's tables only (SQL files)
  internal/store/models.go   or GORM models, auto-migrated (auth does this)
  module.go             New(Deps{DB, Bus, Log, ...}), routes, bus handlers
scripts/demo.sh         curl walkthrough (monolith or microservices)
scripts/check-boundaries.sh
```

## Performance and concurrency

Measured with `go run ./cmd/loadtest` (goroutines as clients) on one laptop
(i7-1165G7, 4 cores / 8 threads, 14 GB) running **the app, Postgres and the
load generator together**, monolith mode, rate limit off. Zero errors in
every run.

| Scenario | Clients | Throughput | p50 | p99 |
|---|---|---|---|---|
| `GET /healthz` (HTTP only) | 50 | 55,000 req/s | 0.7 ms | 4 ms |
| `GET /v1/payments/{id}` (API key auth + DB read) | 50 | 15,300 req/s | 2.6 ms | 12 ms |
| `GET /v1/payments?limit=20` | 50 | 14,900 req/s | 2.8 ms | 12 ms |
| `POST /v1/payments` (auth, merchant lookup, routing + provider, 3 writes) | 50 | 930 req/s | 49 ms | 135 ms |
| `POST /v1/payments` | 200 | 1,420 req/s | 126 ms | 376 ms |
| mixed 80% read / 20% create | 100 | 5,550 req/s | 12 ms | 79 ms |
| mixed 80% read / 20% create | 1,000 | 5,900 req/s | 114 ms | 764 ms |

Run it yourself (server started with `RATE_LIMIT_RPS=0`):
`go run ./cmd/loadtest -url http://localhost:8080 -c 100 -d 20s -scenario mixed -admin-email <admin> -admin-password <password>`
(the admin creates a throwaway merchant; or pass `-key sk_test_...`).

Payment creation is bound by Postgres commits (disk flush per transaction).
On real servers (separate DB host with fast NVMe, more cores) expect several
times more; scale out by running more app replicas (stateless) and moving hot
modules to their own database (`DB_URL_<MODULE>`).

How concurrency is handled:

* **Go's HTTP server runs every request on its own goroutine**; background
  work (outbox relays, webhook delivery, expiry, refund retry, payout
  reservation) runs in goroutines per module, stopped cleanly on shutdown.
* **Database connections are pooled** per module (`DB_MAX_CONNS_PER_MODULE`).
* **Money is protected by the database, not by luck**: unique
  `(merchant, Idempotency-Key)` constraints (50 identical concurrent requests
  → 1 payment), row locks on state changes, a per-payment advisory lock for
  refunds (20 concurrent 10 % refunds → exactly 10 succeed), and a
  per-merchant lock in the ledger for payouts (concurrent payouts never exceed
  the balance). Workers claim jobs with `FOR UPDATE SKIP LOCKED`, so replicas
  never process the same job twice.
* `TestConcurrency` proves these with goroutines on every test run, and the
  whole suite passes under Go's race detector (`go test -race ./...`).

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

**Today only the sandbox adapter is installed**: test mode works for every
method; live methods report `payment_method_unavailable` until a direct
adapter is added. Each adapter is one file implementing `gateway.Gateway`
(`internal/modules/provider/internal/gateway/gateway.go` lists the rules) and
needs the provider's API specification and test credentials. What to request
from each provider: [docs/PROVIDER_ONBOARDING.md](docs/PROVIDER_ONBOARDING.md).

## Not included yet

* Direct provider adapters (telebirr, CBE Birr, M-Pesa, card switch, banks):
  routing, failover and the hosted page are ready; each adapter needs the
  provider's licensed API documents and credentials.
* KYC document upload and review (signup + staff approval exist).
* Automatic payouts through a bank transfer API (today finance sends the
  transfer and marks the payout paid).
* Customer receipts by email; WebAuthn / passkeys as a second factor.
* Disputes / chargebacks, payment links, reports and CSV exports.
* NATS per-service credentials (config only; see Security).
* NATS TLS/credentials config, OpenTelemetry tracing, a shared (Redis)
  rate limiter for multi-replica deployments.
