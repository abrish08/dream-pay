# Architecture

[← back to README](../README.md)

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
