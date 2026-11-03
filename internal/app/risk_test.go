package app_test

import (
	"fmt"
	"strings"
	"testing"
)

func (c *client) merchantID(owner string) string {
	return c.do("GET", "/v1/merchant", owner, "", nil, 200)["id"].(string)
}

func (c *client) setSettings(merchantID string, settings map[string]any) {
	c.t.Helper()
	c.do("PATCH", "/admin/merchants/"+merchantID, c.admin, "", map[string]any{"settings": settings}, 200)
}

func payBody(ref string, amount int64, email string) map[string]any {
	b := map[string]any{"amount": amount, "currency": "ETB", "provider": "mock", "reference": ref}
	if email != "" { // one IP per customer, so only the rule under test fires
		b["customer"] = map[string]string{"email": email, "ip": fmt.Sprintf("196.188.%d.%d", len(email), int(email[0]))}
	}
	return b
}

// TestMerchantLimits: hard limits per merchant, enforced under a lock so
// concurrent payments cannot slip past a daily limit together.
func TestMerchantLimits(t *testing.T) {
	c, _, stop := startApp(t, nil)
	defer stop()
	owner, key := c.signup("limits@shop.test")
	mid := c.merchantID(owner)
	c.do("PATCH", "/admin/merchants/"+mid, c.admin, "", map[string]any{"settings": map[string]any{
		"limits": map[string]any{"ETB": map[string]any{"max_amount": 200000, "daily_amount": 100000}}}}, 400) // max > daily
	c.setSettings(mid, map[string]any{"limits": map[string]any{"ETB": map[string]any{"max_amount": 100000, "daily_count": 5}}})

	e := c.do("POST", "/v1/payments", key, "lim-big", payBody("BIG", 100001, ""), 422)
	if code(e) != "limit_exceeded" || !strings.Contains(fmt.Sprint(e), "max_amount") {
		t.Fatalf("over max_amount: %v", e)
	}
	// 10 at once against a daily count of 5: exactly 5 get through.
	res := parallel(10, func(i int) rawResp {
		return c.raw("POST", "/v1/payments", key, fmt.Sprintf("lim-%d", i), payBody(fmt.Sprintf("L-%d", i), 1000, ""))
	})
	ok, refused := 0, 0
	for _, r := range res {
		switch {
		case r.code == 201:
			ok++
		case r.code == 422 && code(r.body) == "limit_exceeded":
			refused++
		default:
			t.Fatalf("unexpected: %d %v", r.code, r.body)
		}
	}
	if ok != 5 || refused != 5 {
		t.Fatalf("daily_count 5 under concurrency: %d created, %d refused", ok, refused)
	}
	// Settings are visible to staff and the merchant.
	st := c.do("GET", "/v1/merchant", owner, "", nil, 200)["settings"].(map[string]any)
	if st["risk_level"] != "normal" || st["limits"].(map[string]any)["ETB"] == nil {
		t.Fatalf("settings: %v", st)
	}
}

// TestRiskRules: blocklist, customer velocity and repeated declines, every
// decision recorded and visible to staff.
func TestRiskRules(t *testing.T) {
	c, _, stop := startApp(t, nil)
	defer stop()
	_, key := c.signup("risk@shop.test")

	// Blocklist.
	c.do("POST", "/admin/risk/blocklist", c.admin, "", map[string]any{"kind": "email", "value": "Fraud@Example.com", "reason": "chargebacks"}, 201)
	c.do("POST", "/admin/risk/blocklist", c.admin, "", map[string]any{"kind": "email", "value": "fraud@example.com", "reason": "again"}, 409)
	c.do("POST", "/admin/risk/blocklist", c.admin, "", map[string]any{"kind": "ip", "value": "not-an-ip", "reason": "x"}, 400)
	e := c.do("POST", "/v1/payments", key, "risk-1", payBody("R-1", 5000, "fraud@example.com"), 422)
	if code(e) != "risk_declined" {
		t.Fatalf("blocklisted email: %v", e)
	}
	blocked := c.do("GET", "/v1/payments?reference=R-1", key, "", nil, 200)["data"].([]any)[0].(map[string]any)
	if blocked["status"] != "failed" || !strings.Contains(blocked["failure_reason"].(string), "blocklisted_email") {
		t.Fatalf("blocked payment: %v", blocked)
	}
	if got := trail(c.events(key, "/v1/payments/"+blocked["id"].(string)+"/events")); !strings.HasSuffix(got, "pending>failed:risk") {
		t.Fatalf("history: %s", got)
	}

	// Velocity: 5 attempts by one customer in 10 minutes, the 6th is blocked.
	for i := range 5 {
		c.do("POST", "/v1/payments", key, fmt.Sprintf("vel-%d", i), payBody(fmt.Sprintf("V-%d", i), 5000, "busy@example.com"), 201)
	}
	if e := c.do("POST", "/v1/payments", key, "vel-6", payBody("V-6", 5000, "busy@example.com"), 422); code(e) != "risk_declined" {
		t.Fatalf("velocity: %v", e)
	}

	// Repeated declines (card testing) raise the score.
	for i := range 3 {
		p := c.do("POST", "/v1/payments", key, fmt.Sprintf("dec-%d", i), payBody(fmt.Sprintf("D-%d", i), 5000, "tester@example.com"), 201)
		c.checkout(p["checkout_url"].(string), false)
	}
	c.eventually("the 3 declines reached the risk module", func() bool {
		n := 0
		for _, d := range c.do("GET", "/admin/risk/decisions?limit=500", c.admin, "", nil, 200)["data"].([]any) {
			if m := d.(map[string]any); m["email"] == "tester@example.com" && m["failed"] == true {
				n++
			}
		}
		return n == 3
	})
	p := c.do("POST", "/v1/payments", key, "dec-4", payBody("D-4", 5000, "tester@example.com"), 201)
	d := c.do("GET", "/admin/risk/decisions?payment_id="+p["id"].(string), c.admin, "", nil, 200)["data"].([]any)[0].(map[string]any)
	if d["action"] != "allow" || d["score"] != float64(60) || fmt.Sprint(d["reasons"]) != "[repeated_declines]" {
		t.Fatalf("3 declines in an hour: %v", d)
	}
	if n := len(c.do("GET", "/admin/risk/decisions?action=block", c.admin, "", nil, 200)["data"].([]any)); n != 2 {
		t.Fatalf("blocked decisions: %d", n)
	}
	if len(c.do("GET", "/admin/risk/blocklist", c.admin, "", nil, 200)["data"].([]any)) != 1 {
		t.Fatal("blocklist")
	}
}
