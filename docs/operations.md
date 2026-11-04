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
