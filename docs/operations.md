# Operations: alerts and service security

[← back to README](../README.md)

## Alerts

[deployments/prometheus/alerts.yml](../deployments/prometheus/alerts.yml)
holds ready rules (page = wake someone, ticket = working hours), and
[prometheus.example.yml](../deployments/prometheus/prometheus.example.yml) a
scrape config with the `METRICS_TOKEN` bearer token.

| Alert | Severity | Means |
|---|---|---|
| LedgerUnbalanced | page | debits ≠ credits: stop payouts, investigate |
| ReconciliationCritical | page | money differs between us, a provider or the ledger |
| ProviderUnavailable | page | >20% of a provider's calls fail or get no answer |
| HighErrorRate | page | >2% of API requests answer 5xx |
| OutboxBacklog | page | events not published for 2 min (bus down) |
| RiskChecksUnavailable | page | payments accepted without fraud scoring |
| PaygateDown | page | an instance stopped answering |
| PaymentsStuck, PaymentSuccessRateLow, SlowAPI, WebhooksFailing | ticket | degraded service |

Each page needs a runbook: who is on call, the first checks, how to stop
payouts, whom to call at the provider.

## Database connections

Each module keeps its own connection pool (`DB_MAX_CONNS_PER_MODULE`,
default 10). One process running every module uses up to 13 × that, so keep
it below Postgres `max_connections` (100 by default): set
`DB_MAX_CONNS_PER_MODULE=6`, or raise `max_connections`. Too many
connections shows up as `too many clients already` and HTTP 500s under load.

Run the application with an **ordinary database role**, not a superuser:
the ledger and audit log are protected by triggers that only a superuser
can switch off.

## NATS (microservices)

In production every service connects to NATS with **TLS** and **its own
credentials**, whose permissions allow only its own request routes and event
topics:

```
NATS_TLS_CA=/run/secrets/nats-ca.pem
NATS_TLS_CERT=/run/secrets/payment.crt   NATS_TLS_KEY=/run/secrets/payment.key   # mutual TLS (optional)
NATS_CREDS_FILE=/run/secrets/payment.creds
```

Without them the service logs a warning: development only, on a private network.
