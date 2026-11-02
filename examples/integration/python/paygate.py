"""paygate.py: Python 3.8+ client for the paygate API. Standard library only."""
import hashlib
import hmac
import json
import random
import time
import urllib.error
import urllib.parse
import urllib.request

__all__ = ["Paygate", "PaygateError", "WebhookError", "verify_webhook"]


class PaygateError(Exception):
    """An API error. status 0 = network error; code e.g. idempotency_key_reused, kyc_required."""

    def __init__(self, status, body=None):
        err = (body or {}).get("error") or {}
        super().__init__(err.get("message") or f"paygate request failed with HTTP {status}")
        self.status, self.kind, self.code = status, err.get("kind", ""), err.get("code", "")


class WebhookError(Exception):
    """The webhook is not from paygate (bad signature, too old, malformed)."""


class Paygate:
    def __init__(self, api_key, base_url="https://api.paygate.example", timeout=30, max_retries=2):
        if not api_key:
            raise ValueError("paygate: API key is required (server side only)")
        self.api_key, self.base_url = api_key, base_url.rstrip("/")
        self.timeout, self.max_retries = timeout, max_retries

    # ---- Payments ----
    def create_payment(self, params, idempotency_key):
        """amount in minor units (5000 = 50.00); idempotency_key: your order id."""
        return self._request("POST", "/v1/payments", params, _require(idempotency_key))

    def get_payment(self, payment_id):
        return self._request("GET", "/v1/payments/" + urllib.parse.quote(payment_id, safe=""))

    def list_payments(self, **query):
        """status, reference, limit, starting_after, created_from, created_to"""
        return self._request("GET", "/v1/payments" + _qs(query))

    # ---- Refunds ----
    def create_refund(self, params, idempotency_key):
        """params: payment_id, amount (omit = the rest), reason"""
        return self._request("POST", "/v1/refunds", params, _require(idempotency_key))

    def get_refund(self, refund_id):
        return self._request("GET", "/v1/refunds/" + urllib.parse.quote(refund_id, safe=""))

    def get_balance(self, currency="ETB"):
        return self._request("GET", "/v1/balance" + _qs({"currency": currency}))

    def _request(self, method, path, body=None, idempotency_key=None):
        headers = {"Authorization": "Bearer " + self.api_key, "Accept": "application/json"}
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        # Safe to retry: GETs, and POSTs with an Idempotency-Key (never charges twice).
        attempt = 0
        while True:
            req = urllib.request.Request(self.base_url + path, data=data, headers=headers, method=method)
            try:
                with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                    raw = resp.read()
                    return json.loads(raw) if raw else {}
            except urllib.error.HTTPError as e:
                raw = e.read()
                payload = json.loads(raw) if raw else {}
                if (e.code >= 500 or e.code == 429) and attempt < self.max_retries:
                    attempt += 1
                    _sleep(attempt)
                    continue
                raise PaygateError(e.code, payload) from None
            except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
                if attempt < self.max_retries:
                    attempt += 1
                    _sleep(attempt)
                    continue
                raise PaygateError(0, {"error": {"message": "network error: %s" % e}}) from e


def verify_webhook(raw_body, header, secrets, tolerance=300, now=None):
    """Verify a webhook and return the event dict; raises WebhookError.

    raw_body: the request body bytes exactly as received (do not re-serialize JSON).
    header:   the Paygate-Signature header, "t=<unix>,v1=<hex>".
    secrets:  the endpoint secret, or a list of secrets while rotating.
    """
    parts = dict(p.strip().split("=", 1) for p in (header or "").split(",") if "=" in p)
    try:
        ts = int(parts.get("t", ""))
        given = bytes.fromhex(parts.get("v1", ""))
    except ValueError:
        raise WebhookError("malformed signature header") from None
    if len(given) != 32:
        raise WebhookError("malformed signature header")
    if abs((time.time() if now is None else now) - ts) > tolerance:
        raise WebhookError("signature timestamp outside tolerance")
    if isinstance(raw_body, str):
        raw_body = raw_body.encode()
    if isinstance(secrets, str):
        secrets = [secrets]
    for secret in secrets:
        expected = hmac.new(secret.encode(), str(ts).encode() + b"." + raw_body, hashlib.sha256).digest()
        if hmac.compare_digest(expected, given):
            return json.loads(raw_body)
    raise WebhookError("signature mismatch")


def _require(key):
    if not key:
        raise ValueError("paygate: an Idempotency-Key is required (use your order id)")
    return key


def _qs(q):
    q = {k: v for k, v in q.items() if v not in (None, "")}
    return "?" + urllib.parse.urlencode(q) if q else ""


def _sleep(attempt):
    time.sleep(0.3 * 2 ** (attempt - 1) + random.random() / 10)
