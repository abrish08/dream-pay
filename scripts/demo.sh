#!/usr/bin/env bash
# End-to-end walkthrough of the gateway using the mock provider.
#
#   API=http://localhost:8080 ADMIN_EMAIL=... ADMIN_PASSWORD=... ./scripts/demo.sh
#
# API is either the monolith or the edge gateway in front of separate
# services; the script is identical for both.
set -euo pipefail

API=${API:-http://localhost:8080}
ADMIN_EMAIL=${ADMIN_EMAIL:?set ADMIN_EMAIL (the bootstrap admin)}
ADMIN_PASSWORD=${ADMIN_PASSWORD:?set ADMIN_PASSWORD}
WEBHOOK_URL=${WEBHOOK_URL:-}

say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }
fail() { echo "FAILED: $*" >&2; exit 1; }
admin() { curl -sS -H "Authorization: Bearer $ADMIN_TOKEN" -H 'Content-Type: application/json' "$@"; }
owner() { curl -sS -H "Authorization: Bearer $OWNER_TOKEN" -H 'Content-Type: application/json' "$@"; }
api()   { curl -sS -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' "$@"; }
login() { curl -sS -H 'Content-Type: application/json' -X POST "$API/auth/login" -d "{\"email\":\"$1\",\"password\":\"$2\"}"; }

wait_for() { # wait_for <description> <command that prints a value> <expected>
  for _ in $(seq 1 50); do
    got=$(eval "$2")
    [[ "$got" == "$3" ]] && { echo "$1: $got"; return 0; }
    sleep 0.2
  done
  fail "$1: expected $3, got $got"
}

say "0. Platform admin logs in"
ADMIN_TOKEN=$(login "$ADMIN_EMAIL" "$ADMIN_PASSWORD" | jq -r .access_token)
[[ "$ADMIN_TOKEN" != null ]] || fail "admin login"
echo "admin logged in (access token, 15 min)"

say "1. Admin onboards a merchant (fee 1.5%) and its owner account"
MERCHANT=$(admin -X POST "$API/admin/merchants" \
  -d "{\"name\":\"Abebe Shop\",\"email\":\"shop+$RANDOM$RANDOM@example.com\",\"fee_bps\":150}")
echo "$MERCHANT" | jq .
MID=$(echo "$MERCHANT" | jq -r .id)
OWNER_EMAIL="owner+$RANDOM$RANDOM@example.com"
OWNER_PASSWORD="owner-password-$RANDOM$RANDOM"
admin -X POST "$API/admin/users" -d "{\"email\":\"$OWNER_EMAIL\",\"name\":\"Abebe\",\"password\":\"$OWNER_PASSWORD\",\"role\":\"owner\",\"merchant_id\":\"$MID\"}" \
  | jq '{id, email, role, permissions}'

say "2. Merchant owner logs in and creates a test API key (secret shown once)"
OWNER_TOKEN=$(login "$OWNER_EMAIL" "$OWNER_PASSWORD" | jq -r .access_token)
KEYRES=$(owner -X POST "$API/v1/api-keys" \
  -d '{"name":"demo","environment":"test","scopes":["payments:read","payments:write","refunds:write","balance:read","webhooks:write"]}')
echo "$KEYRES" | jq '{id, prefix, last4, scopes, warning}'
KEY=$(echo "$KEYRES" | jq -r .secret)
[[ "$KEY" == sk_test_* ]] || fail "create API key"

if [[ -n "$WEBHOOK_URL" ]]; then
  say "3. Register a webhook endpoint"
  api -X POST "$API/v1/webhook-endpoints" -d "{\"url\":\"$WEBHOOK_URL\",\"events\":[\"*\"]}" | jq '{id, url, events}'
fi

say "4. Create a payment of 1,500.00 ETB"
IDEM="order-$RANDOM$RANDOM"
BODY='{"amount":150000,"currency":"ETB","provider":"mock","reference":"INV-1042","customer":{"email":"buyer@example.com","name":"Sara Bekele"}}'
PAY=$(api -X POST "$API/v1/payments" -H "Idempotency-Key: $IDEM" -d "$BODY")
echo "$PAY" | jq '{id, status, amount, fee, checkout_url}'
PID=$(echo "$PAY" | jq -r .id)
CHECKOUT=$(echo "$PAY" | jq -r .checkout_url)
[[ $(echo "$PAY" | jq -r .status) == processing ]] || fail "payment not processing"

say "5. Retry with the same Idempotency-Key -> same payment, no duplicate"
code=$(api -o /dev/null -w '%{http_code}' -X POST "$API/v1/payments" -H "Idempotency-Key: $IDEM" -d "$BODY")
again=$(api -X POST "$API/v1/payments" -H "Idempotency-Key: $IDEM" -d "$BODY" | jq -r .id)
echo "HTTP $code, id $again"
[[ "$again" == "$PID" && "$code" == 200 ]] || fail "idempotent replay"
conflict=$(api -X POST "$API/v1/payments" -H "Idempotency-Key: $IDEM" -d '{"amount":1,"currency":"ETB","provider":"mock","reference":"X"}' | jq -r .error.code)
echo "same key, different body -> $conflict"
[[ "$conflict" == idempotency_key_reused ]] || fail "idempotency conflict"

say "6. Customer pays on the provider's checkout page"
curl -sS -o /dev/null -w 'checkout page HTTP %{http_code}\n' -X POST -d result=success "$CHECKOUT"

say "7. Events flow: provider -> payment -> ledger / webhook"
wait_for "payment status" "api $API/v1/payments/$PID | jq -r .status" succeeded
wait_for "merchant balance (1500.00 - 22.50 fee)" "api '$API/v1/balance?currency=ETB' | jq -r .available" 147750

say "8. Partial refund of 300.00 ETB"
REF=$(api -X POST "$API/v1/refunds" -H "Idempotency-Key: refund-$RANDOM$RANDOM" \
  -d "{\"payment_id\":\"$PID\",\"amount\":30000,\"reason\":\"item returned\"}")
echo "$REF" | jq '{id, status, amount}'
[[ $(echo "$REF" | jq -r .status) == succeeded ]] || fail "refund"
over=$(api -X POST "$API/v1/refunds" -H "Idempotency-Key: refund-$RANDOM$RANDOM" \
  -d "{\"payment_id\":\"$PID\",\"amount\":200000}" | jq -r .error.message)
echo "over-refund rejected: $over"

wait_for "payment status" "api $API/v1/payments/$PID | jq -r .status" partially_refunded
wait_for "merchant balance after refund" "api '$API/v1/balance?currency=ETB' | jq -r .available" 117750

say "9. Ledger integrity (debits must equal credits)"
admin "$API/admin/ledger/trial-balance" | jq .

say "10. Security checks"
echo "no key        -> $(curl -s -o /dev/null -w '%{http_code}' "$API/v1/payments")"
echo "invalid key   -> $(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer sk_test_nope' "$API/v1/payments")"
echo "other's data  -> $(api -o /dev/null -w '%{http_code}' "$API/v1/payments/00000000-0000-0000-0000-000000000000")"
echo "key makes key -> $(api -o /dev/null -w '%{http_code}' -X POST "$API/v1/api-keys" -d '{"name":"x","scopes":["payments:read"]}')"
echo "owner -> admin-> $(owner -o /dev/null -w '%{http_code}' "$API/admin/merchants/$MID")"

say "Done. Payment $PID"
