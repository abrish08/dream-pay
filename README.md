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
| **merchant** | `merchants.GetMerchant`, `CreateMerchant` | – | `MerchantUpdated` | `kyc.StatusChanged` (approval activates) |
| **auth** | `auth.ExchangeAPIKey`, `RegisterPermissions` | `GetMerchant`, `CreateMerchant` (signup) | `AccessChanged` | `MerchantUpdated` |
| **provider** | `providers.InitiateCheckout`, `VerifyTransaction`, `Refund` | – | `providers.TransactionUpdated` | – |
| **payment** | `payments.GetPayment`, `ListPayments` | `GetMerchant`, `InitiateCheckout`, `VerifyTransaction` (expiry) | `PaymentSucceeded`, `PaymentFailed`, `PaymentExpired` | `TransactionUpdated`, `RefundSucceeded` |
| **refund** | – | `GetPayment`, `providers.Refund` | `RefundSucceeded`, `RefundFailed` | – |
| **ledger** | `ledger.ReserveFunds` | – | – | `PaymentSucceeded`, `RefundSucceeded`, `PayoutPaid`, `PayoutFailed` |
| **payout** | – | `GetMerchant`, `ReserveFunds` | `PayoutPaid`, `PayoutFailed` | – |
| **kyc** | – | `GetMerchant` (email) | `kyc.StatusChanged` | – |
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
  **`merchant`** (the default role of every new merchant account; its
  permissions are seeded once from `MERCHANT_DEFAULT_PERMISSIONS` and then
  chosen by platform staff), `owner` (every merchant permission, locked),
  `admin`, `developer`, `analyst`, `viewer`. Platform staff can edit the non-locked ones; merchants
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
* Webhooks cannot be aimed at internal systems (SSRF): URLs must be `https`,
  without credentials, and not point at loopback, private, link-local or cloud
  metadata addresses (`169.254.169.254`) or internal names (`localhost`,
  `*.internal`). The address is checked again at connect time, after DNS, so
  a public name that resolves to an internal IP is refused too. Delivery
  never follows redirects and ignores proxy settings.
  `WEBHOOK_ALLOW_INSECURE=true` lifts these checks (development only).
* KYC documents: private bucket, short-lived signed links, 5 MB limit (see
  "Merchant verification").

## Tests

```bash
go test -race ./...                                   # unit tests
# end-to-end (needs a THROWAWAY database whose name contains "test": it is wiped)
createdb paygate_test
TEST_DATABASE_URL=postgres://postgres:<password>@localhost:5432/paygate_test?sslmode=disable \
  go test -race -count=1 ./internal/app/
# business scenarios only, narrated step by step
TEST_DATABASE_URL=… go test -count=1 ./internal/app/ -run TestScenarios -v
./scripts/check-boundaries.sh                         # no module may import another
```

**Scenario tests** (`internal/app/scenarios_test.go`) run real business stories
on one running instance, across every service:

| # | Scenario |
|---|---|
| S01 | Merchant signs up, verifies email, sells in test mode, passes KYC, issues a live key and takes a live payment |
| S02 | Customer pays an order with telebirr; signed `payment.succeeded` webhook with the order metadata; balance net of fee |
| S03 | Customer chooses the method on the hosted checkout page (CBE Birr) |
| S04 | Customer declines: payment fails, nothing credited, refund impossible |
| S05 | Abandoned checkout expires (`payment.expired`); a late payment is still honoured |
| S06 | Refunds: partial, provider decline, over-refund refused, "refund the rest" |
| S07 | Settlement: payout held, paid, a failed transfer released; payout webhooks |
| S08 | Network retries never double-charge (idempotency replay and conflict) |
| S09 | Merchant invites a developer with limited access |
| S10 | Merchant's webhook endpoint is down, then recovers (retry) |
| S11 | Forgot password: email reset ends every old session |
| S12 | Suspension blocks a merchant everywhere; reactivation restores it; no KYC bypass |
| S13 | Staff change what every merchant may do (default role), effective within seconds |
| S14 | Reconciliation completes and the ledger balances after all of the above |

Other end-to-end tests cover every endpoint with its Swagger sample, the docs
covering every route, concurrency races (same idempotency key, parallel
refunds and payouts), rate limits, KYC, MFA, Google/Apple sign-in and pepper
rotation. `TEST_NATS_URL` adds the cross-process bus test.

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

## Default merchant role

Every new merchant account (signup, Google/Apple, or created by staff without
a role) gets the built-in **`merchant`** role. It is seeded once, like the
first admin, from `MERCHANT_DEFAULT_PERMISSIONS` in `.env`; after that
platform staff decide what merchants may do:

```
GET    /admin/roles?audience=merchant                → find the "merchant" role id
POST   /admin/roles/{id}/permissions  {"permissions": ["payouts:write"]}   add
DELETE /admin/roles/{id}/permissions/refunds:write                          remove
```

Changes apply to **every merchant** within seconds (their tokens become stale
and refresh with the new permissions). Merchants cannot edit this shared role;
they can create their own custom roles for their team, within the permissions
they hold.

## Merchant verification (KYC)

A self-signed-up merchant starts **pending**: test mode works, live mode does
not. To go live it completes the business verification:

```
PUT  /v1/kyc/profile        business type, legal name, TIN (10 digits), trade license number
                            and expiry, registration number, VAT, applicant role, address, owner
POST /v1/kyc/fayda/start    owner signs in with Fayda (national digital ID) → details locked
POST /v1/kyc/documents      multipart: type + file (PDF / JPEG / PNG, max 5 MB)
POST /v1/kyc/submit         422 kyc_incomplete lists anything missing
        ↓  staff: GET /admin/kyc?status=submitted → review, open documents
POST /admin/kyc/{merchantId}/approve   → kyc.status_changed → merchant becomes ACTIVE
POST /admin/kyc/{merchantId}/reject    {reason} → merchant fixes and resubmits
```

### Documents (Ethiopian requirements)

`GET /v1/kyc` → `required_documents` is computed from the profile:

| Document | Required for |
|---|---|
| `trade_license` (valid; expired licenses block submission) | every business except NGOs |
| `commercial_registration` | every business except NGOs |
| `registration_certificate` (ACSO) | NGOs |
| `tin_certificate` | everyone |
| `memorandum_of_association`, `shareholders_list` | PLC, share company, partnership |
| `vat_certificate` | `vat_registered: true` (with `vat_number`) |
| `business_address_proof` (lease or title deed) | everyone |
| `bank_account_proof` (bank letter or statement) | everyone |
| `owner_id` (passport / residence permit) | owner **not** verified with Fayda |
| `power_of_attorney` | `applicant_role: authorized_representative` |
| `investment_permit` | `foreign_owned: true` |
| `certificate_of_competence` | `regulated_sector: true` |

Files: PDF, JPEG or PNG, **at most 5 MB** (checked on the request body and on
the file; larger → `413 file_too_large`). The type is detected from the bytes,
not the name.

### Owner identity with Fayda

With `owner.id_type: "fayda"` the owner proves who they are through Fayda's
eSignet (OpenID Connect), instead of uploading an ID scan:

```
POST /v1/kyc/fayda/start  → {authorize_url}     dashboard opens it
  owner signs in at Fayda (OTP to their Fayda phone) and consents
GET  /kyc/fayda/callback  ← Fayda (state + one-time code)
  → <DASHBOARD_URL>/kyc?fayda=verified   (or ?fayda=failed&reason=...)
```

* Client authentication with a signed JWT (`private_key_jwt`, RS256) — no
  shared secret; PKCE (S256), `state` and `nonce` per attempt; sessions are
  single use and expire in 10 minutes; the ID token and the signed user info
  are verified against Fayda's published keys.
* Name, birth date, gender, phone, nationality, address and photo come from
  Fayda and **lock** the owner fields (`owner_identity_locked`); under-18 owners
  are refused; the national ID number is masked when shown; everything is
  encrypted in the database. Staff see the Fayda photo next to the documents.
* Setup: register as a relying party at partner.fayda.et (NBE-licensed
  institutions go through ETSwitch), register the public key and the
  redirect URI `<PUBLIC_BASE_URL>/kyc/fayda/callback`, then set
  `FAYDA_CLIENT_ID` and `FAYDA_PRIVATE_KEY`. Without them Fayda is off
  (`fayda_available: false`) and owners upload `owner_id`.

### Storage: object keys in the database, links on read

```
upload  → MinIO  kyc-documents/kyc/<merchantId>/<documentId>.pdf   (private bucket, SSE)
DB      → object_key = "kyc/<merchantId>/<documentId>.pdf"         (never a URL)
read    → GET /v1/kyc, /v1/kyc/documents/{id}
          → pre-signed URL built now, valid 5 minutes
```

* `KYC_STORAGE=minio` (production): any S3-compatible store. The bucket is
  created private at start-up; no public policy is ever set. With
  `KYC_S3_SSE=true` objects are encrypted by the server (MinIO KMS).
  `MINIO_PUBLIC_ENDPOINT` is the host in links (e.g. `files.example.com`
  behind TLS), `MINIO_ENDPOINT` the internal address. Use an access key
  limited to the bucket.
* `KYC_STORAGE=local` (development): files encrypted with AES-256-GCM in
  `KYC_STORAGE_DIR`, each bound to its key; links are HMAC-signed, expiring
  `/kyc/files/{token}` URLs served by the gateway.
* Why keys, not URLs: a link leaked from a log or an email stops working in
  minutes, access can be revoked by deleting the object, and the bucket, host
  or CDN can change without touching a single database row. Keys are built
  only from ids and validated (`kyc/<uuid>/<uuid>.pdf|jpg|png`), so they can
  never point outside the KYC prefix.

### Rules

* **Test mode never needs KYC; live mode always does.** Live API keys, live
  payments and live payouts require an approved verification for every
  merchant, including merchants created by staff (`403 kyc_required`).
  KYC can be submitted at any time. Platform staff never go through KYC.
  Approval also activates a pending (self-signed-up) merchant; staff cannot
  activate it without approved KYC (`409 kyc_not_approved`). Merchants that
  were live before KYC existed are marked `waived`.
* The owner's personal data and the Fayda identity are encrypted in the
  database; a SHA-256 of each file is kept to prove which file was approved.
* **Everything is recorded** in `kyc_history` (uploads, Fayda verifications
  and failures, submissions, every staff document view, decisions with
  reasons). The merchant is emailed on submit, approval and rejection.
* Permissions: merchants `kyc:read` / `kyc:write` (owner, admin; logged-in users
  only, never API keys); staff `kyc:review` (operations by default).

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
from each provider: [docs/PROVIDER_ONBOARDING.md](docs/PROVIDER_ONBOARDING.md).

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

## Not included yet

* Direct provider adapters for telebirr, CBE Birr, M-Pesa and banks (cards
  are done: Mastercard Gateway); each needs the provider's licensed API
  documents and credentials.
* Currency conversion (pricing in USD, settling in ETB): USD is processed and
  kept as USD today.
* KYC checks against business registries (ERCA TIN lookup, MoTRI trade
  registry) and sanctions / PEP screening: staff verify the documents
  manually (the owner's identity can already be verified with Fayda).
* Automatic payouts through a bank transfer API (today finance sends the
  transfer and marks the payout paid).
* Customer receipts by email; WebAuthn / passkeys as a second factor.
* Disputes / chargebacks, payment links, reports and CSV exports.
* NATS per-service credentials (config only; see Security).
* NATS TLS/credentials config, OpenTelemetry tracing, a shared (Redis)
  rate limiter for multi-replica deployments.
