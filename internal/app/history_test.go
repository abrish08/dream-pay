package app_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"paygate/internal/platform/config"
)

func fastChecks(cfg *config.Config) {
	cfg.PaymentCheckEvery = 100 * time.Millisecond
	cfg.PaymentCheckSchedule = []time.Duration{300 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond,
		300 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond, 300 * time.Millisecond}
}

// events returns "from>to:source" for each history entry.
func (c *client) events(token, path string) []map[string]any {
	c.t.Helper()
	var out []map[string]any
	for _, e := range c.do("GET", path, token, "", nil, 200)["data"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

func trail(evs []map[string]any) string {
	var parts []string
	for _, e := range evs {
		parts = append(parts, fmt.Sprintf("%s>%s:%s", e["from_status"], e["to_status"], e["source"]))
	}
	return strings.Join(parts, " ")
}

// TestPaymentHistory: every status change is recorded with who caused it and
// why, in the same transaction, and the history cannot be rewritten.
func TestPaymentHistory(t *testing.T) {
	c, _, stop := startApp(t, fastChecks)
	defer stop()
	_, key := c.signup("history@shop.test")
	pid := c.pay(key, "hist-1", 10000)
	c.do("POST", "/v1/refunds", key, "hist-r1", map[string]any{"payment_id": pid, "amount": 3000, "reason": "one item"}, 201)

	var evs []map[string]any
	c.eventually("refund in the history", func() bool {
		evs = c.events(key, "/v1/payments/"+pid+"/events")
		return strings.HasSuffix(trail(evs), "succeeded>partially_refunded:refund")
	})
	want := ">pending:merchant_api pending>processing:merchant_api processing>succeeded:provider succeeded>partially_refunded:refund"
	if got := trail(evs); got != want {
		t.Fatalf("history:\n got  %s\n want %s", got, want)
	}
	if evs[2]["provider"] != "mock" || evs[2]["details"].(map[string]any)["provider_status"] != "succeeded" {
		t.Fatalf("provider answer not kept: %v", evs[2])
	}

	// Only its merchant (and staff) can read it.
	_, other := c.signup("other-history@shop.test")
	c.do("GET", "/v1/payments/"+pid+"/events", other, "", nil, 404)
	if len(c.events(c.admin, "/admin/payments/"+pid+"/events")) != 4 {
		t.Fatal("staff view of the history")
	}
	c.do("GET", "/admin/payments/"+pid, c.admin, "", nil, 200)

	// Append-only, enforced by the database itself.
	err := openTestDB(t).Exec("UPDATE payment.payment_events SET reason = 'rewritten' WHERE payment_id = ?", pid).Error
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("history was rewritten: %v", err)
	}
	if err := openTestDB(t).Exec("DELETE FROM payment.payment_events WHERE payment_id = ?", pid).Error; err == nil {
		t.Fatal("history was deleted")
	}
}

// TestProviderOutcomeUnknown: a provider that does not answer is never
// treated as a failure (the customer may be paying); the status checks find
// the real result. A provider that certainly did not get the request fails
// the payment at once.
func TestProviderOutcomeUnknown(t *testing.T) {
	c, _, stop := startApp(t, fastChecks)
	defer stop()
	_, key := c.signup("unknown@shop.test")

	// ...9.98: connection refused, nothing sent.
	c.do("POST", "/v1/payments", key, "down-1", map[string]any{"amount": 10998, "currency": "ETB",
		"provider": "mock", "reference": "DOWN-1"}, 502)
	down := c.do("GET", "/v1/payments?reference=DOWN-1", key, "", nil, 200)["data"].([]any)[0].(map[string]any)
	if down["status"] != "failed" || !strings.Contains(down["failure_reason"].(string), "provider_initiation_failed") {
		t.Fatalf("unreachable provider: %v", down)
	}

	// ...9.99: the request reached the provider but no answer came back.
	p := c.do("POST", "/v1/payments", key, "unknown-1", map[string]any{"amount": 20999, "currency": "ETB",
		"provider": "mock", "reference": "UNKNOWN-1"}, 201)
	pid := p["id"].(string)
	if p["status"] != "processing" || p["checkout_url"] != nil && p["checkout_url"] != "" {
		t.Fatalf("unknown outcome must keep the payment open: %v", p)
	}
	if again := c.do("POST", "/v1/payments", key, "unknown-1", map[string]any{"amount": 20999, "currency": "ETB",
		"provider": "mock", "reference": "UNKNOWN-1"}, 200); again["id"] != pid {
		t.Fatal("retry created a second payment")
	}
	time.Sleep(time.Second) // a few status checks: the provider still says pending
	if s := c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)["status"]; s != "processing" {
		t.Fatalf("still unknown, must stay processing: %v", s)
	}

	// The customer approves on their phone; the provider's callback is lost.
	if err := openTestDB(t).Exec("UPDATE provider.mock_transactions SET status = 'succeeded' WHERE tx_ref = ?", pid).Error; err != nil {
		t.Fatal(err)
	}
	c.eventually("status check finds the payment", func() bool {
		return c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)["status"] == "succeeded"
	})
	got := trail(c.events(key, "/v1/payments/"+pid+"/events"))
	if got != ">pending:merchant_api pending>processing:merchant_api processing>succeeded:status_check" {
		t.Fatalf("history: %s", got)
	}
	evs := c.events(key, "/v1/payments/"+pid+"/events")
	if !strings.Contains(evs[1]["reason"].(string), "provider_outcome_unknown") {
		t.Fatalf("why it stayed open: %v", evs[1])
	}
}
