# paygate

A payment gateway for Ethiopia, written in Go. Merchants take payments with
telebirr, CBE Birr, M-Pesa, cards (Visa, Mastercard) and bank transfer, in
ETB or USD, through one API.

It is a **modular monolith**: twelve modules that never import each other and
talk only through a central bus. The same binary runs as one process
(`MODULES=all BUS=memory`) or as separate services (`MODULES=payment BUS=nats`),
with no code changes.

## Features

- **Payments**: hosted checkout, idempotent create, expiry, refunds, metadata, search
- **Providers**: routing per payment method with failover and circuit breaker; Mastercard Gateway (cards, USD) built in
- **Money**: double-entry ledger per currency, merchant balances, payouts, daily three-way reconciliation with settlement files
- **Correctness**: append-only payment history, timeouts never treated as failures, scheduled status checks with providers
- **Risk**: hard merchant limits, fraud rules (blocklist, velocity, card testing, unusual amounts), rolling reserves
- **Disputes**: chargebacks with evidence, deadlines and ledger holds; scheduled payouts; Prometheus metrics
- **Merchants**: self-signup, KYC with Ethiopian document rules, Fayda identity verification, MinIO document storage
- **Accounts**: email/SMS verification, Google and Apple sign-in, authenticator app (TOTP), password reset, dynamic RBAC
- **Integration**: API keys (test/live), signed webhooks with retries, OpenAPI docs, client code for Node.js, PHP, Python, Java and Go
- **Safety**: outbox pattern, rate limits, SSRF guard, encrypted secrets and personal data, security headers

## Quick start

Needs Go 1.24+ and PostgreSQL.

```bash
./scripts/run.sh env      # creates .env with generated secrets, prints the first admin's password
# edit .env: DATABASE_URL=postgres://<user>:<password>@localhost:5432/gateway?sslmode=disable
./scripts/run.sh db       # creates the database (tables are created on start)
./scripts/run.sh          # starts on http://localhost:8080
./scripts/run.sh demo     # in another terminal: a full walkthrough against the running server
```

Then open **http://localhost:8080/swagger** to try the API (`/docs` for the reference).

With Docker: `docker compose --profile mono up --build` (one process) or
`--profile micro` (one service per module, with NATS).

`.env` is git-ignored and must never be committed; `.env.example` holds placeholders only.

## Documentation

| Topic | |
|---|---|
| [Architecture](docs/architecture.md) | modules, the bus, payment flow, microservices, migrations, layout, performance |
| [Authentication and RBAC](docs/auth.md) | sign-in, verification, MFA, roles and permissions, API keys |
| [Merchant verification (KYC)](docs/kyc.md) | required documents, Fayda, document storage |
| [Payment providers and cards](docs/providers.md) | routing, Mastercard Gateway, going live, local simulator |
| [History, status checks, reconciliation](docs/reconciliation.md) | payment audit trail, timeouts, daily three-way reconciliation, settlement files |
| [Risk, reserves, payouts, disputes, monitoring](docs/risk-and-money.md) | limits, fraud rules, rolling reserve, scheduled payouts, chargebacks, metrics |
| [Merchant integration guides](docs/integration/README.md) | Node.js, PHP, Python, Java, Go |
| [API summary](docs/api.md) | endpoints at a glance (full reference at `/docs`) |
| [Security](docs/security.md) | how secrets, webhooks and data are protected |
| [Tests](docs/testing.md) | unit, end-to-end and scenario tests |
| [Provider onboarding](docs/PROVIDER_ONBOARDING.md) | what to request from each payment provider |

## Tests

```bash
go test ./...                                   # unit tests
TEST_DATABASE_URL=postgres://.../paygate_test go test -race ./...   # everything (the database is wiped: name must contain "test")
```

## Not included yet

- Direct adapters for telebirr, CBE Birr, M-Pesa and banks (need each provider's API documents and credentials)
- Disputes received automatically from providers (today staff record them from the provider's notice)
- Automatic bank transfers for payouts (finance sends the transfer and marks it paid)
- Currency conversion (USD is processed and kept as USD)
- Registry checks (ERCA, MoTRI) and sanctions screening in KYC
- Payment links, merchant reports; NATS TLS and per-service credentials, tracing, a shared rate limiter for many replicas
