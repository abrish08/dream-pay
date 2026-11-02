package com.example.shop;

// Spring Boot (spring-boot-starter-web) example: "Buy" -> paygate checkout -> webhook marks the order paid.
// Add Paygate.java to your project (or the jar built from this folder) and set the three environment variables.

import com.paygate.Paygate;
import org.springframework.http.ResponseEntity;
import org.springframework.web.bind.annotation.*;

import java.net.URI;
import java.util.List;
import java.util.Map;

@RestController
public class ShopController {
    private final Paygate paygate = new Paygate(System.getenv("PAYGATE_API_KEY"), System.getenv("PAYGATE_BASE_URL"));
    private final String webhookSecret = System.getenv("PAYGATE_WEBHOOK_SECRET");
    private final String shopUrl = "https://shop.example";

    // 1. The customer clicks "Buy": create the payment on the server, redirect to checkout_url.
    @PostMapping("/checkout")
    public ResponseEntity<Void> checkout() {
        String orderId = "ORDER-" + System.currentTimeMillis(); // save as "pending" in your database
        try {
            Map<String, Object> payment = paygate.createPayment(Map.of(
                    "amount", 5000,                 // 50.00
                    "currency", "USD",
                    "payment_method", "card",       // or leave out: the customer chooses
                    "reference", orderId,
                    "description", "2 bags of coffee",
                    "customer", Map.of("email", "buyer@example.com"),
                    "return_url", shopUrl + "/thanks?order=" + orderId), orderId); // idempotency key = order id
            return ResponseEntity.status(303).location(URI.create((String) payment.get("checkout_url"))).build();
        } catch (Paygate.PaygateException e) {
            // e.status, e.code (e.g. kyc_required, payment_method_unavailable), e.getMessage()
            return ResponseEntity.status(502).build();
        }
    }

    // 2. Webhook: take the RAW body (byte[]), verify, then update the order once.
    @PostMapping("/webhooks/paygate")
    @SuppressWarnings("unchecked")
    public ResponseEntity<String> webhook(@RequestBody byte[] raw,
                                          @RequestHeader(value = "Paygate-Signature", defaultValue = "") String signature) {
        Map<String, Object> event;
        try {
            event = Paygate.verifyWebhook(raw, signature, List.of(webhookSecret), 300, System.currentTimeMillis() / 1000);
        } catch (Paygate.WebhookException e) {
            return ResponseEntity.badRequest().body("invalid signature");
        }
        Map<String, Object> data = (Map<String, Object>) event.get("data");
        String orderId = (String) data.get("reference");
        // Skip event.get("id") if already processed (events can arrive more than once).
        switch ((String) event.get("type")) {
            case "payment.succeeded": /* mark orderId paid, ship */ break;
            case "payment.failed":    /* mark failed: data.get("reason"), e.g. insufficient_funds */ break;
            case "payment.expired":   /* not paid in time */ break;
            default: break;
        }
        return ResponseEntity.ok("ok"); // answer 2xx quickly, or paygate retries
    }
}
