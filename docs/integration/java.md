# Java integration

Java 11 or newer. One dependency: Jackson (`jackson-databind`) for JSON; HTTP
uses the JDK's `HttpClient`. Works with Spring Boot, Jakarta EE or plain Java.
Read [How a payment works](README.md#how-a-payment-works) first.

## 1. Add the client

Copy [`Paygate.java`](../../examples/integration/java/src/main/java/com/paygate/Paygate.java)
into your project and add Jackson (Spring Boot already includes it):

```xml
<dependency>
  <groupId>com.fasterxml.jackson.core</groupId>
  <artifactId>jackson-databind</artifactId>
  <version>2.17.2</version>
</dependency>
```

Create one client and reuse it (it is thread-safe):

```java
Paygate paygate = new Paygate(System.getenv("PAYGATE_API_KEY"), System.getenv("PAYGATE_BASE_URL"));
```

```bash
PAYGATE_API_KEY=sk_test_...         # server only, never in a frontend or app
PAYGATE_WEBHOOK_SECRET=whsec_...
PAYGATE_BASE_URL=https://api.paygate.example
```

## 2. Create a payment and redirect the customer

Spring Boot ([`ShopController.java`](../../examples/integration/java/spring/ShopController.java)):

```java
@PostMapping("/checkout")
public ResponseEntity<Void> checkout(Principal user) {
    Order order = orders.create(5000);                 // status "pending"
    try {
        Map<String, Object> payment = paygate.createPayment(Map.of(
                "amount", order.getTotal(),            // minor units: 5000 = 50.00
                "currency", "ETB",
                "payment_method", "telebirr",          // or "card", "cbe_birr", ...; leave out to let the customer choose
                "reference", order.getId(),
                "return_url", "https://shop.example/thanks?order=" + order.getId()),
            order.getId());                            // Idempotency-Key = your order id
        return ResponseEntity.status(303).location(URI.create((String) payment.get("checkout_url"))).build();
    } catch (Paygate.PaygateException e) {
        log.error("paygate {} {}: {}", e.status, e.code, e.getMessage());
        return ResponseEntity.status(502).build();
    }
}
```

## 3. The customer comes back

Show "payment is being confirmed" until the webhook has marked the order
paid. Do not mark it paid from the return URL.

## 4. Receive webhooks

Take the body as `byte[]` so it is not re-encoded:

```java
@PostMapping("/webhooks/paygate")
public ResponseEntity<String> webhook(@RequestBody byte[] raw,
                                      @RequestHeader(value = "Paygate-Signature", defaultValue = "") String signature) {
    Map<String, Object> event;
    try {
        event = Paygate.verifyWebhook(raw, signature, List.of(webhookSecret), 300, System.currentTimeMillis() / 1000);
    } catch (Paygate.WebhookException e) {
        return ResponseEntity.badRequest().body("invalid signature");
    }
    if (events.alreadyProcessed((String) event.get("id"))) return ResponseEntity.ok("ok");

    Map<String, Object> data = (Map<String, Object>) event.get("data");
    String orderId = (String) data.get("reference");
    switch ((String) event.get("type")) {
        case "payment.succeeded": orders.markPaid(orderId); break;
        case "payment.failed":    orders.markFailed(orderId, (String) data.get("reason")); break;
        case "payment.expired":   orders.markExpired(orderId); break;
        default: break;
    }
    return ResponseEntity.ok("ok");                    // answer quickly
}
```

With Spring Security, allow this path without login and without CSRF; the
signature protects it. While rotating the secret pass both:
`List.of(oldSecret, newSecret)`.

## 5. Status, refunds, balance

```java
Map<String, Object> p = paygate.getPayment(paymentId);      // p.get("status"), p.get("failure_reason")
Map<String, Object> page = paygate.listPayments(Map.of("reference", "ORDER-1001", "limit", "20"));
Map<String, Object> refund = paygate.createRefund(
        Map.of("payment_id", paymentId, "amount", 2000, "reason", "one item returned"), // omit amount = the rest
        "refund-" + orderId + "-1");
Map<String, Object> balance = paygate.getBalance("ETB");    // balance.get("available")
```

## Errors

```java
try {
    paygate.createPayment(params, orderId);
} catch (Paygate.PaygateException e) {
    switch (e.code) {
        case "payment_method_unavailable": /* offer another method */ break;
        case "kyc_required":               /* live mode: finish business verification */ break;
        default: log.error("paygate {} {}: {}", e.status, e.code, e.getMessage());
    }
}
```

Network errors, `429` and `5xx` are retried automatically (2 retries) with the
same Idempotency-Key.

## Test it

```bash
cd examples/integration/java
mvn test                                                                     # webhook verification
PAYGATE_BASE_URL=http://localhost:8080 PAYGATE_API_KEY=sk_test_... mvn test   # also the live API
```
