"""python3 -m unittest   (live tests need PAYGATE_BASE_URL and PAYGATE_API_KEY with a test key)"""
import json
import os
import time
import unittest

from paygate import Paygate, PaygateError, WebhookError, verify_webhook

HERE = os.path.dirname(os.path.abspath(__file__))
with open(os.path.join(HERE, "../../../docs/integration/testdata/webhook-vector.json")) as f:
    V = json.load(f)


class Webhooks(unittest.TestCase):
    def test_published_vector(self):
        e = verify_webhook(V["body"].encode(), V["header"], V["secret"], now=V["timestamp"] + 10)
        self.assertEqual(e["type"], "payment.succeeded")
        self.assertEqual(e["data"]["reference"], "ORDER-1001")

    def test_rotation(self):
        self.assertTrue(verify_webhook(V["body"], V["header"], ["whsec_old", V["secret"]], now=V["timestamp"]))

    def test_rejects(self):
        t = V["timestamp"]
        for body, header, secret, now in [
            (V["body"].replace("5000", "9000"), V["header"], V["secret"], t),
            (V["body"], V["header"], "whsec_wrong", t),
            (V["body"], V["header"], V["secret"], t + 301),
            (V["body"], "garbage", V["secret"], t),
        ]:
            with self.assertRaises(WebhookError):
                verify_webhook(body, header, secret, now=now)


@unittest.skipUnless(os.environ.get("PAYGATE_BASE_URL") and os.environ.get("PAYGATE_API_KEY"), "live API not configured")
class Live(unittest.TestCase):
    def test_payment_flow(self):
        pg = Paygate(os.environ["PAYGATE_API_KEY"], base_url=os.environ["PAYGATE_BASE_URL"])
        ref = "PY-%d" % (time.time() * 1000)
        params = {"amount": 5000, "currency": "USD", "payment_method": "card", "reference": ref}
        p = pg.create_payment(params, ref)
        self.assertTrue(p["checkout_url"].startswith("http"))
        self.assertEqual(pg.create_payment(params, ref)["id"], p["id"])
        self.assertEqual(pg.get_payment(p["id"])["reference"], ref)
        self.assertTrue(any(x["id"] == p["id"] for x in pg.list_payments(reference=ref)["data"]))
        with self.assertRaises(PaygateError) as cm:
            pg.create_payment(dict(params, amount=1), ref)
        self.assertEqual(cm.exception.code, "idempotency_key_reused")
        with self.assertRaises(PaygateError) as cm:
            pg.get_payment("00000000-0000-0000-0000-000000000000")
        self.assertEqual(cm.exception.status, 404)


if __name__ == "__main__":
    unittest.main()
