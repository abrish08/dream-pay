package app_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"paygate/internal/platform/config"
)

// TestMetrics: Prometheus metrics for traffic, payments, providers and the
// health of background work, behind a token.
func TestMetrics(t *testing.T) {
	c, _, stop := startApp(t, func(cfg *config.Config) { cfg.MetricsToken = "scrape-secret" })
	defer stop()
	_, key := c.signup("metrics@shop.test")
	c.pay(key, "met-1", 5000)
	c.eventually("ledger booked", func() bool { // the ledger gauge needs entries
		return c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"] == float64(4925)
	})

	if r, _ := http.Get(c.base + "/metrics"); r.StatusCode != 401 {
		t.Fatalf("metrics without token: %d", r.StatusCode)
	}
	req, _ := http.NewRequest("GET", c.base+"/metrics", nil)
	req.Header.Set("Authorization", "Bearer scrape-secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(body)
	for _, want := range []string{
		`paygate_http_requests_total{method="POST",route="/v1/payments",status="201"}`,
		`paygate_http_request_duration_seconds_bucket{le="+Inf",method="POST",route="/v1/payments"}`,
		`paygate_payments_created_total{currency="ETB",environment="test",method=""}`,
		`paygate_payments_final_total{environment="test",provider="mock",reason="",status="succeeded"}`,
		`paygate_provider_requests_total{operation="initiate",outcome="ok",provider="mock"}`,
		`paygate_payment_risk_total{outcome="allow"}`,
		`paygate_outbox_pending{module="payment",stat="count"}`,
		`paygate_ledger_unbalanced{currency="ETB"} 0`,
		"# TYPE paygate_webhook_deliveries gauge",
		"# TYPE paygate_reconciliation_open_items gauge",
		"# TYPE paygate_disputes_open gauge",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics miss %s", want)
		}
	}
	if strings.Contains(text, "/v1/payments/") && strings.Contains(text, "route=\"/v1/payments/"+"c") {
		t.Fatal("raw paths with ids must not become labels")
	}
}
