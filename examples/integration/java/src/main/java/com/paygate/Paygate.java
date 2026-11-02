package com.paygate;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.ObjectMapper;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;
import java.io.IOException;
import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.time.Duration;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ThreadLocalRandom;
import java.util.stream.Collectors;

/**
 * Java 11+ client for the paygate API. Server side only: the API key must
 * never reach a browser or a mobile app. Thread-safe; create one and reuse it.
 */
public final class Paygate {
    private static final ObjectMapper JSON = new ObjectMapper();
    private static final TypeReference<Map<String, Object>> MAP = new TypeReference<>() {};

    private final String apiKey;
    private final String baseUrl;
    private final int maxRetries;
    private final HttpClient http;

    public Paygate(String apiKey, String baseUrl) {
        this(apiKey, baseUrl, 2, Duration.ofSeconds(30));
    }

    public Paygate(String apiKey, String baseUrl, int maxRetries, Duration timeout) {
        if (apiKey == null || apiKey.isEmpty()) throw new IllegalArgumentException("paygate: API key is required");
        this.apiKey = apiKey;
        this.baseUrl = baseUrl.replaceAll("/+$", "");
        this.maxRetries = maxRetries;
        this.http = HttpClient.newBuilder().connectTimeout(timeout).followRedirects(HttpClient.Redirect.NEVER).build();
    }

    /** API error. status 0 = network error; code e.g. idempotency_key_reused, kyc_required. */
    public static final class PaygateException extends RuntimeException {
        public final int status;
        public final String kind;
        public final String code;

        PaygateException(int status, String kind, String code, String message) {
            super(message);
            this.status = status;
            this.kind = kind;
            this.code = code;
        }
    }

    /** The webhook is not from paygate. */
    public static final class WebhookException extends Exception {
        WebhookException(String message) { super(message); }
    }

    // ---- Payments ----

    /** params: amount (minor units, 5000 = 50.00), currency, reference, payment_method, return_url, ... */
    public Map<String, Object> createPayment(Map<String, Object> params, String idempotencyKey) {
        return request("POST", "/v1/payments", params, requireKey(idempotencyKey));
    }

    public Map<String, Object> getPayment(String id) {
        return request("GET", "/v1/payments/" + enc(id), null, null);
    }

    /** query: status, reference, limit, starting_after, created_from, created_to */
    public Map<String, Object> listPayments(Map<String, String> query) {
        String qs = query.entrySet().stream().map(e -> enc(e.getKey()) + "=" + enc(e.getValue())).collect(Collectors.joining("&"));
        return request("GET", "/v1/payments" + (qs.isEmpty() ? "" : "?" + qs), null, null);
    }

    // ---- Refunds ----

    /** params: payment_id, amount (omit = the rest), reason */
    public Map<String, Object> createRefund(Map<String, Object> params, String idempotencyKey) {
        return request("POST", "/v1/refunds", params, requireKey(idempotencyKey));
    }

    public Map<String, Object> getBalance(String currency) {
        return request("GET", "/v1/balance?currency=" + enc(currency), null, null);
    }

    private Map<String, Object> request(String method, String path, Object body, String idempotencyKey) {
        HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(baseUrl + path))
                .timeout(Duration.ofSeconds(30))
                .header("Authorization", "Bearer " + apiKey)
                .header("Accept", "application/json");
        try {
            if (body != null) {
                b.header("Content-Type", "application/json")
                        .method(method, HttpRequest.BodyPublishers.ofString(JSON.writeValueAsString(body)));
            } else {
                b.method(method, HttpRequest.BodyPublishers.noBody());
            }
        } catch (IOException e) {
            throw new IllegalArgumentException(e);
        }
        if (idempotencyKey != null) b.header("Idempotency-Key", idempotencyKey);
        HttpRequest req = b.build();

        // Safe to retry: GETs, and POSTs with an Idempotency-Key (never charges twice).
        for (int attempt = 0; ; attempt++) {
            HttpResponse<String> resp;
            try {
                resp = http.send(req, HttpResponse.BodyHandlers.ofString());
            } catch (IOException e) {
                if (attempt < maxRetries) { backoff(attempt); continue; }
                throw new PaygateException(0, "", "network_error", "network error: " + e.getMessage());
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                throw new PaygateException(0, "", "interrupted", "interrupted");
            }
            int status = resp.statusCode();
            Map<String, Object> data = parse(resp.body());
            if (status >= 200 && status < 300) return data;
            if ((status >= 500 || status == 429) && attempt < maxRetries) { backoff(attempt); continue; }
            @SuppressWarnings("unchecked")
            Map<String, Object> err = (Map<String, Object>) data.getOrDefault("error", Map.of());
            throw new PaygateException(status, str(err.get("kind")), str(err.get("code")),
                    err.containsKey("message") ? str(err.get("message")) : "paygate request failed with HTTP " + status);
        }
    }

    // ---- Webhooks ----

    /**
     * Verify a webhook and return the event (id, type, created, data).
     *
     * @param rawBody   the request body bytes exactly as received (not re-serialized JSON)
     * @param header    the Paygate-Signature header, "t=<unix>,v1=<hex>"
     * @param secrets   the endpoint secret (several while rotating)
     * @param tolerance max signature age (use 300 seconds)
     * @param nowEpoch  current time in seconds (System.currentTimeMillis() / 1000)
     */
    public static Map<String, Object> verifyWebhook(byte[] rawBody, String header, List<String> secrets,
                                                    long tolerance, long nowEpoch) throws WebhookException {
        Map<String, String> parts = new HashMap<>();
        for (String p : (header == null ? "" : header).split(",")) {
            String[] kv = p.trim().split("=", 2);
            if (kv.length == 2) parts.put(kv[0], kv[1]);
        }
        String t = parts.getOrDefault("t", ""), v1 = parts.getOrDefault("v1", "");
        if (!t.matches("\\d{1,12}") || !v1.matches("[0-9a-f]{64}")) throw new WebhookException("malformed signature header");
        long ts = Long.parseLong(t);
        if (Math.abs(nowEpoch - ts) > tolerance) throw new WebhookException("signature timestamp outside tolerance");
        byte[] given = hex(v1);
        for (String secret : secrets) {
            try {
                Mac mac = Mac.getInstance("HmacSHA256");
                mac.init(new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
                mac.update((t + ".").getBytes(StandardCharsets.UTF_8));
                if (MessageDigest.isEqual(mac.doFinal(rawBody), given)) return parse(new String(rawBody, StandardCharsets.UTF_8));
            } catch (java.security.GeneralSecurityException e) {
                throw new IllegalStateException(e);
            }
        }
        throw new WebhookException("signature mismatch");
    }

    // ---- helpers ----

    private static Map<String, Object> parse(String s) {
        if (s == null || s.isEmpty()) return new HashMap<>();
        try {
            return JSON.readValue(s, MAP);
        } catch (IOException e) {
            throw new PaygateException(0, "", "invalid_response", "unreadable response from paygate");
        }
    }

    private static String requireKey(String k) {
        if (k == null || k.isEmpty()) throw new IllegalArgumentException("paygate: an Idempotency-Key is required (use your order id)");
        return k;
    }

    private static String enc(String s) { return URLEncoder.encode(s, StandardCharsets.UTF_8).replace("+", "%20"); }

    private static String str(Object o) { return o == null ? "" : o.toString(); }

    private static byte[] hex(String s) {
        byte[] out = new byte[s.length() / 2];
        for (int i = 0; i < out.length; i++) out[i] = (byte) Integer.parseInt(s.substring(2 * i, 2 * i + 2), 16);
        return out;
    }

    private static void backoff(int attempt) {
        try {
            Thread.sleep(300L * (1L << attempt) + ThreadLocalRandom.current().nextInt(100));
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }
}
