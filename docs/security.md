# Security

[← back to README](../README.md)


* Webhook secrets: AES-256-GCM encrypted (they must be read back to sign).
* Provider callbacks are always re-verified with the provider's API.
* In production, secure NATS with TLS and per-service credentials (each
  service may only answer its own routes and publish its own events), and
  keep it on a private network.
* Webhooks cannot be aimed at internal systems (SSRF): URLs must be `https`,
  without credentials, and not point at loopback, private, link-local or cloud
  metadata addresses (`169.254.169.254`) or internal names (`localhost`,
  `*.internal`). The address is checked again at connect time, after DNS, so
  a public name that resolves to an internal IP is refused too. Delivery
  never follows redirects and ignores proxy settings.
  `WEBHOOK_ALLOW_INSECURE=true` lifts these checks (development only).
* KYC documents: private bucket, short-lived signed links, 5 MB limit (see
  "Merchant verification").
