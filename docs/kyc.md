# Merchant verification (KYC)

[← back to README](../README.md)


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
