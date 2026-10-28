# Provider onboarding: what to request, what we build

paygate routes each **payment method** to a **provider adapter**. Routing,
failover, the hosted checkout page, refunds, the ledger, reconciliation and
webhooks are already built. What is missing for live money is one adapter per
provider, and each adapter needs the provider's documents and credentials
listed below.

> The lists below are what payment integrations typically require. Each
> provider and the National Bank of Ethiopia have the final word: ask them
> for their current requirements and confirm with legal counsel.

## 1. Before any provider: licence and company documents

Direct integrations (telebirr, CBE Birr, M-Pesa, card switch, banks) normally
require the gateway operator itself to be authorised. Prepare or request:

| Item | From | Why |
|---|---|---|
| NBE licence / authorisation for operating a payment system or acting as a payment aggregator (the category that matches the business model) | National Bank of Ethiopia | Required to collect funds for merchants and settle to them |
| Commercial registration, business licence, TIN, memorandum of association | Ministry of Trade / revenue authority | Every provider asks for these |
| AML/CFT programme, compliance officer, merchant KYC policy | Internal (+ Financial Intelligence Service registration if required) | Providers and NBE check how merchants are vetted |
| Settlement (trust / collection) bank account | Partner bank | Where provider settlements arrive and payouts leave from |
| Information security policy, data-protection measures, incident process | Internal | Asked during provider due diligence |
| Card payments only: PCI DSS scope assessment | QSA / acquirer | Hosted card pages keep paygate out of most PCI scope |

## 2. Request from each provider

Ask every provider (telebirr / Ethio Telecom, CBE Birr / Commercial Bank of
Ethiopia, M-Pesa / Safaricom Ethiopia, EthSwitch for domestic cards, the
acquiring bank for Visa/Mastercard, and the banks for transfers) for:

**Commercial**
- [ ] Partner / aggregator agreement that allows serving **sub-merchants**
      (your merchants), not only your own sales
- [ ] Fees (MDR), settlement cycle (T+1, T+2…) and settlement account rules
- [ ] Transaction limits (minimum, maximum, daily) and allowed currencies

**Technical (needed to write the adapter)**
- [ ] API specification for: create payment / checkout, query status,
      refund or reversal (full and partial, time limits), and if available
      settlement / transaction reports
- [ ] Customer flow: redirect page, USSD push, in-app / QR, or OTP; and
      what the customer sees on failure
- [ ] Callback (notification) specification: payload, how it is signed
      (signature algorithm, certificates or keys), retry policy, expected
      response
- [ ] Authentication of our API calls: app id / key / secret, token flow,
      request signing (e.g. RSA keys), mutual TLS certificates
- [ ] IP allow-listing on both sides (we send our server's outgoing IPs; they
      send theirs for callbacks)
- [ ] Complete error-code list, with which codes are final declines and which
      are temporary
- [ ] Idempotency rules (can a request be retried with the same reference?)
- [ ] Reconciliation file format and delivery (SFTP / API, daily time)
- [ ] **Sandbox / UAT access**: base URL, test credentials, test phone numbers
      or test cards, and their UAT test cases
- [ ] Certification / go-live checklist, then production credentials

**What we give them**
- Callback URL: `https://<your-domain>/providers/<provider-name>/callback`
- Return URL for customers: the merchant's `return_url` (per payment)
- Our outgoing IP addresses, public keys or certificates they ask for
- Company and licence documents from section 1

## 3. Suggested order (hybrid plan)

1. **telebirr**: the largest wallet; usually the biggest share of volume.
2. **CBE Birr**: largest bank wallet.
3. **M-Pesa**.
4. **Cards**: EthSwitch (domestic) and an acquiring bank (Visa/Mastercard).
5. **Bank transfers**.

Until a method's direct integration is live you can route it to any licensed
partner you sign with (a backup provider), then switch with one line of
`PROVIDER_ROUTES`; merchants notice nothing.

## 4. Building an adapter (developer checklist)

Once a provider's documents and sandbox credentials arrive:

1. Add `internal/modules/provider/internal/gateway/<provider>.go` implementing
   `gateway.Gateway`. The rules are documented at the top of `gateway.go`:
   idempotent `Initiate` / `Refund`, `Verify` as the source of truth,
   authenticated `ParseCallback`, "unavailable" errors only for outages.
2. Add its settings (`<PROVIDER>_BASE_URL`, keys…) to `config` and
   `.env.example` as `CHANGE_ME` placeholders; secrets come from a secret
   manager in production.
3. Register it in `internal/modules/provider/module.go`.
4. Tests against recorded sandbox responses, including declines, timeouts and
   duplicate callbacks.
5. Run the provider's UAT cases, then set
   `PROVIDER_ROUTES=telebirr=telebirr` (and a backup if any) in production.
6. Watch `GET /admin/providers` (health, failures) and the daily
   reconciliation runs.
