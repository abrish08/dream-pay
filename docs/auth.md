# Authentication, accounts and RBAC

[← back to README](../README.md)

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

Every completed sign-in returns an **access token** (JWT, carries roles and
permissions) and a **refresh token** (stored hashed, rotated on every use).
Their lifetimes come from `.env`:

| Setting | Default | Allowed | Meaning |
|---|---|---|---|
| `AUTH_ACCESS_TOKEN_TTL` | `5m` | 1m to 1h | role changes and sign-out take effect within this time |
| `AUTH_REFRESH_TOKEN_TTL` | `168h` (7 days) | 1h to 2160h | how long someone stays signed in without signing in again |

The dashboard refreshes once for all requests that arrive together (a used
refresh token is never sent twice, which would sign the user out everywhere)
and treats only token errors as "signed out"; a wrong authenticator code is
shown as an error on the page.

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

## Two-factor authentication (authenticator app)

Every user (merchant or staff) can protect their account with Microsoft
Authenticator, Google Authenticator or any TOTP app:

```
POST /auth/mfa/totp/setup     → qr_code (PNG data URL to scan), secret (to type), issuer, account
POST /auth/mfa/totp/confirm   {code} → enabled + 10 recovery codes (shown once)
POST /auth/login              → {mfa_required, mfa_token}
POST /auth/login/mfa          {mfa_token, code | recovery code} → tokens
```

* **One secret per user**, generated on the server, stored **encrypted**
  (AES-256-GCM); the QR code is drawn on the server (never by a third party).
* The app shows **"Paygate - <business name>"** for merchant users and
  **"Paygate Admin"** for staff, with the user's email.
* Codes: 6 digits, 30 s, ±1 step; a used code cannot be replayed; wrong codes
  count towards the login lockout. Recovery codes are stored hashed and work
  once.
* The session remembers whether it was signed in with MFA (also across
  refreshes) and the access token says so (`mfa: true`). Enabling MFA does not
  upgrade sessions that already exist: sign in again.

| Setting (default) | Enforced on |
|---|---|
| `REQUIRE_STAFF_MFA=true` | every `/admin` route |
| `REQUIRE_MERCHANT_MFA=true` | every `/v1` route for dashboard users (API keys are not affected) |
| `REQUIRE_MFA_FOR_PAYOUTS=true` | `POST /v1/payouts` |

Without MFA these answer `403 mfa_required`; enrolment (`/auth/mfa/...`) is
always reachable.
