"""Example shop: "Buy" -> paygate checkout -> webhook marks the order paid (standard library only).

PAYGATE_API_KEY=sk_test_... PAYGATE_WEBHOOK_SECRET=whsec_... PAYGATE_BASE_URL=http://localhost:8080 python3 shop.py
"""
import os
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

from paygate import Paygate, PaygateError, WebhookError, verify_webhook

paygate = Paygate(os.environ["PAYGATE_API_KEY"], base_url=os.environ.get("PAYGATE_BASE_URL", "https://api.paygate.example"))
SHOP = os.environ.get("SHOP_URL", "http://localhost:3001")
orders = {}  # use your database


class Shop(BaseHTTPRequestHandler):
    def do_POST(self):
        url = urlparse(self.path)
        if url.path == "/checkout":  # 1. create the payment on the server, redirect to checkout_url
            order_id = "ORDER-%d" % (time.time() * 1000)
            orders[order_id] = {"status": "pending", "events": set()}
            try:
                payment = paygate.create_payment({
                    "amount": 5000, "currency": "USD", "payment_method": "card",
                    "reference": order_id, "description": "2 bags of coffee",
                    "customer": {"email": "buyer@example.com"},
                    "return_url": "%s/thanks?order=%s" % (SHOP, order_id),
                }, idempotency_key=order_id)
            except PaygateError as e:
                return self._send(502, "payment service error: %s %s" % (e.status, e.code))
            self.send_response(303)
            self.send_header("Location", payment["checkout_url"])
            self.end_headers()
        elif url.path == "/webhooks/paygate":  # 3. verify on the RAW body, update once
            raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            try:
                event = verify_webhook(raw, self.headers.get("Paygate-Signature"), os.environ["PAYGATE_WEBHOOK_SECRET"])
            except WebhookError:
                return self._send(400, "invalid signature")
            order = orders.get(event["data"].get("reference"))
            if order and event["id"] not in order["events"]:  # events can arrive more than once
                order["events"].add(event["id"])
                order["status"] = {"payment.succeeded": "paid", "payment.failed": "failed",
                                   "payment.expired": "expired"}.get(event["type"], order["status"])
            self._send(200, "ok")  # answer 2xx quickly, or paygate retries
        else:
            self._send(404, "")

    def do_GET(self):  # 2. back from checkout: show "processing" until the webhook says paid
        url = urlparse(self.path)
        if url.path == "/thanks":
            order = orders.get(parse_qs(url.query).get("order", [""])[0])
            paid = order and order["status"] == "paid"
            return self._send(200, "Thank you, payment received." if paid else "Payment is being confirmed...")
        self._send(404, "")

    def _send(self, code, text):
        self.send_response(code)
        self.end_headers()
        self.wfile.write(text.encode())

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    print("shop on", SHOP)
    ThreadingHTTPServer(("127.0.0.1", urlparse(SHOP).port or 3001), Shop).serve_forever()
