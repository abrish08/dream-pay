package com.paygate;

import com.fasterxml.jackson.databind.ObjectMapper;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.condition.EnabledIfEnvironmentVariable;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.util.List;
import java.util.Map;

import static org.junit.jupiter.api.Assertions.*;

/** mvn test   (live test needs PAYGATE_BASE_URL and PAYGATE_API_KEY with a test key) */
class PaygateTest {
    @SuppressWarnings("unchecked")
    private static Map<String, Object> vector() throws Exception {
        return new ObjectMapper().readValue(Files.readString(Path.of("../../../docs/integration/testdata/webhook-vector.json")), Map.class);
    }

    @Test
    @SuppressWarnings("unchecked")
    void verifiesPublishedVector() throws Exception {
        Map<String, Object> v = vector();
        byte[] body = ((String) v.get("body")).getBytes(StandardCharsets.UTF_8);
        String header = (String) v.get("header"), secret = (String) v.get("secret");
        long t = ((Number) v.get("timestamp")).longValue();

        Map<String, Object> e = Paygate.verifyWebhook(body, header, List.of(secret), 300, t + 10);
        assertEquals("payment.succeeded", e.get("type"));
        assertEquals("ORDER-1001", ((Map<String, Object>) e.get("data")).get("reference"));
        assertNotNull(Paygate.verifyWebhook(body, header, List.of("whsec_old", secret), 300, t));

        byte[] tampered = ((String) v.get("body")).replace("5000", "9000").getBytes(StandardCharsets.UTF_8);
        assertThrows(Paygate.WebhookException.class, () -> Paygate.verifyWebhook(tampered, header, List.of(secret), 300, t));
        assertThrows(Paygate.WebhookException.class, () -> Paygate.verifyWebhook(body, header, List.of("whsec_wrong"), 300, t));
        assertThrows(Paygate.WebhookException.class, () -> Paygate.verifyWebhook(body, header, List.of(secret), 300, t + 301));
        assertThrows(Paygate.WebhookException.class, () -> Paygate.verifyWebhook(body, "garbage", List.of(secret), 300, t));
    }

    @Test
    @EnabledIfEnvironmentVariable(named = "PAYGATE_API_KEY", matches = "sk_test_.+")
    void livePaymentFlow() {
        Paygate pg = new Paygate(System.getenv("PAYGATE_API_KEY"), System.getenv("PAYGATE_BASE_URL"));
        String ref = "JAVA-" + System.currentTimeMillis();
        Map<String, Object> params = Map.of("amount", 5000, "currency", "USD", "payment_method", "card", "reference", ref);
        Map<String, Object> p = pg.createPayment(params, ref);
        assertTrue(((String) p.get("checkout_url")).startsWith("http"));
        assertEquals(p.get("id"), pg.createPayment(params, ref).get("id"));
        assertEquals(ref, pg.getPayment((String) p.get("id")).get("reference"));
        Paygate.PaygateException e = assertThrows(Paygate.PaygateException.class,
                () -> pg.createPayment(Map.of("amount", 1, "currency", "USD", "reference", ref), ref));
        assertEquals("idempotency_key_reused", e.code);
    }
}
