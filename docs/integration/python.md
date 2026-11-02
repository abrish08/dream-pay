# Python integration

Python 3.8 or newer. No dependencies: the client uses the standard library
only, so it works with Django, Flask, FastAPI or plain scripts. Read
[How a payment works](README.md#how-a-payment-works) first.

## 1. Add the client

Copy [`paygate.py`](../../examples/integration/python/paygate.py) into your
project and configure it once:

```python
import os
from paygate import Paygate, PaygateError, WebhookError, verify_webhook

paygate = Paygate(os.environ["PAYGATE_API_KEY"], base_url=os.environ["PAYGATE_BASE_URL"])
```

```bash
# server environment (never commit it)
PAYGATE_API_KEY=sk_test_...
PAYGATE_WEBHOOK_SECRET=whsec_...
PAYGATE_BASE_URL=https://api.paygate.example
```

## 2. Create a payment and redirect the customer

Django:

```python
from django.shortcuts import redirect
from django.http import HttpResponse

def checkout(request):
    order = Order.objects.create(total=5000, status="pending")
    try:
        payment = paygate.create_payment({
            "amount": order.total,            # minor units: 5000 = 50.00
            "currency": "ETB",
            "payment_method": "telebirr",     # or "card", "cbe_birr", ...; leave out to let the customer choose
            "reference": str(order.id),
            "customer": {"email": request.user.email},
            "return_url": f"https://shop.example/thanks?order={order.id}",
        }, idempotency_key=str(order.id))     # Idempotency-Key = your order id
    except PaygateError as e:
        logger.error("paygate %s %s: %s", e.status, e.code, e)
        return HttpResponse("Payment is unavailable, please try again.", status=502)
    order.payment_id = payment["id"]
    order.save()
    return redirect(payment["checkout_url"])
```

Flask is the same: `return redirect(payment["checkout_url"], code=303)`.

## 3. The customer comes back

Show "payment is being confirmed" until the webhook has marked the order
paid. Do not mark it paid from the return URL.

## 4. Receive webhooks

Use the **raw body** (`request.body` in Django, `request.get_data()` in Flask):

```python
from django.views.decorators.csrf import csrf_exempt
from django.views.decorators.http import require_POST

@csrf_exempt          # paygate cannot send your CSRF token; the signature protects this view
@require_POST
def paygate_webhook(request):
    try:
        event = verify_webhook(request.body, request.headers.get("Paygate-Signature", ""),
                               os.environ["PAYGATE_WEBHOOK_SECRET"])
    except WebhookError:
        return HttpResponse("invalid signature", status=400)

    _, created = ProcessedEvent.objects.get_or_create(event_id=event["id"])
    if not created:
        return HttpResponse("ok")             # already handled

    order = Order.objects.get(id=event["data"]["reference"])
    if event["type"] == "payment.succeeded":
        order.status = "paid"
    elif event["type"] == "payment.failed":
        order.status, order.failure_reason = "failed", event["data"].get("reason")
    elif event["type"] == "payment.expired":
        order.status = "expired"
    order.save()
    return HttpResponse("ok")                 # answer quickly
```

Flask:

```python
@app.post("/webhooks/paygate")
def paygate_webhook():
    try:
        event = verify_webhook(request.get_data(), request.headers.get("Paygate-Signature", ""),
                               os.environ["PAYGATE_WEBHOOK_SECRET"])
    except WebhookError:
        return "invalid signature", 400
    # ...same handling as above...
    return "ok"
```

While rotating the secret pass both: `verify_webhook(body, header, [old_secret, new_secret])`.

## 5. Status, refunds, balance

```python
p = paygate.get_payment(payment_id)                      # p["status"], p.get("failure_reason")
page = paygate.list_payments(reference="ORDER-1001", limit=20)
refund = paygate.create_refund({"payment_id": payment_id, "amount": 2000, "reason": "one item returned"},
                               idempotency_key=f"refund-{order.id}-1")   # omit amount = refund the rest
balance = paygate.get_balance("ETB")                     # balance["available"]
```

## Errors

```python
try:
    paygate.create_payment(params, idempotency_key=order_id)
except PaygateError as e:
    if e.code == "payment_method_unavailable":
        ...  # offer another method
    elif e.code == "kyc_required":
        ...  # live mode: finish business verification
    else:
        logger.error("paygate %s %s: %s", e.status, e.code, e)
```

Network errors, `429` and `5xx` are retried automatically (2 retries) with the
same Idempotency-Key.

## Test it

```bash
cd examples/integration/python
python3 -m unittest                                                             # webhook verification
PAYGATE_BASE_URL=http://localhost:8080 PAYGATE_API_KEY=sk_test_... python3 -m unittest   # also the live API
```

A complete runnable shop is in [`shop.py`](../../examples/integration/python/shop.py).
