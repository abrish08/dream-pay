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
./scripts/run.sh env      # creates .env from .env.example (dev keys included)
# edit .env: DATABASE_URL=postgres://<user>:<password>@localhost:5432/gateway?sslmode=disable
./scripts/run.sh db       # creates the "gateway" database (tables are created on start)
./scripts/run.sh          # starts on http://localhost:8080

# in another terminal: login, onboarding, API key, payment, refund, security checks
API=http://localhost:8080 ADMIN_EMAIL=admin@paygate.local ADMIN_PASSWORD=change-me-admin-password ./scripts/demo.sh
```

With `make` installed: `make env`, `make db`, `make run`, `make demo`.

Everything in Docker:

```bash
docker compose --profile mono  up --build     # monolith
docker compose --profile micro up --build     # 8 services + 8 databases + NATS + edge gateway
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
| **merchant** | `merchants.GetMerchant` | – | `MerchantUpdated` | – |
| **auth** | `auth.ExchangeAPIKey` | `GetMerchant` (only for a merchant it has not seen) | – | `MerchantUpdated` |
| **provider** | `providers.InitiateCheckout`, `VerifyTransaction`, `Refund` | – | `providers.TransactionUpdated` | – |
| **payment** | `payments.GetPayment`, `ListPayments` | `GetMerchant`, `InitiateCheckout` | `PaymentSucceeded`, `PaymentFailed` | `TransactionUpdated`, `RefundSucceeded` |
| **refund** | – | `GetPayment`, `providers.Refund` | `RefundSucceeded`, `RefundFailed` | – |
| **ledger** | – | – | – | `PaymentSucceeded`, `RefundSucceeded` |
| **webhook** | – | – | – | payment.\*, refund.\* |
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

| Method | Path | Who / permission | Service |
|---|---|---|---|
| POST | `/auth/login`, `/auth/refresh`, `/auth/logout` | none | auth |
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

## Not included yet

* Email invitations / password reset, 2FA (TOTP), SSO for staff.
* NATS per-service credentials (config only; see Security).
* More provider adapters (telebirr, CBE Birr): implement `gateway.Gateway` in
  `internal/modules/provider/internal/gateway/` and register it in `module.go`.
  The Chapa adapter follows Chapa's public API; confirm endpoints and the
  webhook signature header against their docs before going live.
* NATS TLS/credentials config, per-key rate limiting, OpenTelemetry tracing.
* Merchant payouts/settlement (a new module consuming ledger events).
