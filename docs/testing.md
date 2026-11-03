# Tests

[← back to README](../README.md)


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
