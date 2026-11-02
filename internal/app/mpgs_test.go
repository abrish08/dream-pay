package app_test

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"paygate/internal/modules/provider/mpgstest"
	"paygate/internal/platform/config"
)

// TestMastercardUSD: a verified merchant takes a live USD card payment through
// the Mastercard Gateway (Hosted Checkout), then refunds part of it.
func TestMastercardUSD(t *testing.T) {
	bank := mpgstest.New("DASHEN000123", "live-api-password")
	defer bank.Close()
	sandbox := mpgstest.New("TESTDASHEN000123", "test-api-password")
	defer sandbox.Close()
	c, _, stop := startApp(t, func(cfg *config.Config) {
		cfg.MPGS = &config.MPGSSettings{BaseURL: bank.URL, MerchantID: bank.MerchantID, APIPassword: bank.Password,
			APIVersion: "100", MerchantName: "Paygate", NotificationSecret: "notify-secret", Currencies: []string{"USD"}, HTTP: bank.Client()}
		cfg.MPGSTest = &config.MPGSSettings{BaseURL: sandbox.URL, MerchantID: sandbox.MerchantID, APIPassword: sandbox.Password,
			APIVersion: "100", Currencies: []string{"USD", "ETB"}, HTTP: sandbox.Client()}
		cfg.ProviderRoutes = map[string][]string{"card": {"mpgs"}}
		cfg.ProviderTestRoutes = map[string][]string{"card": {"mpgs_test"}}
	})
	defer stop()
	owner, testKey := c.signup("usd@shop.test")
	mid := c.do("GET", "/v1/merchant", owner, "", nil, 200)["id"].(string)
	c.passKYC(owner, mid)
	var live string
	c.eventually("live key", func() bool {
		r := c.raw("POST", "/v1/api-keys", owner, "", map[string]any{"name": "live", "environment": "live",
			"scopes": []string{"payments:read", "payments:write", "refunds:write", "balance:read"}})
		if r.code == 201 {
			live = r.body["secret"].(string)
		}
		return r.code == 201
	})

	// Card is offered for USD; ETB cards are not (the merchant account settles USD only).
	methods := c.do("GET", "/v1/payment-methods?currency=USD", live, "", nil, 200)["data"].([]any)
	cardOK := false
	for _, m := range methods {
		m := m.(map[string]any)
		cardOK = cardOK || (m["method"] == "card" && m["available"] == true)
		if m["method"] == "telebirr" && m["available"] == true {
			t.Fatalf("telebirr has no live provider: %v", m)
		}
	}
	if !cardOK {
		t.Fatalf("card for USD: %v", methods)
	}
	c.do("POST", "/v1/payments", live, "usd-etb", map[string]any{"amount": 5000, "currency": "ETB", "payment_method": "card", "reference": "ETB-CARD"}, 422)

	p := c.do("POST", "/v1/payments", live, "usd-1", map[string]any{"amount": 5000, "currency": "USD",
		"payment_method": "card", "reference": "ORDER-USD-1", "customer": map[string]string{"email": "buyer@example.com"},
		"return_url": "https://shop.example/thanks"}, 201)
	pid := p["id"].(string)
	checkout, _ := url.Parse(p["checkout_url"].(string))
	if checkout.Path != "/providers/mpgs/pay/"+pid || checkout.Query().Get("session") == "" {
		t.Fatalf("checkout url: %v", p["checkout_url"])
	}

	// Our page loads Mastercard's script with the session; strict CSP.
	resp, err := http.Get(p["checkout_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if resp.StatusCode != 200 || !strings.Contains(string(page), bank.URL+"/static/checkout/checkout.min.js") ||
		!strings.Contains(string(page), checkout.Query().Get("session")) || !strings.Contains(string(page), "showPaymentPage") ||
		!strings.Contains(csp, "script-src 'nonce-") || !strings.Contains(csp, bank.URL) || strings.Contains(csp, "cdn.jsdelivr") {
		t.Fatalf("card page: %d %s\nCSP: %s", resp.StatusCode, page, csp)
	}
	if r, _ := http.Get(c.base + "/providers/mpgs/pay/" + pid + "?session=<script>"); r.StatusCode != 404 {
		t.Fatalf("bad session id accepted: %d", r.StatusCode)
	}

	// Customer pays on Mastercard's page, comes back: verified with MPGS, sent to the shop.
	back := bank.Pay(checkout.Query().Get("session"), true)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := noFollow.Get(back)
	if err != nil || r.StatusCode != 303 || r.Header.Get("Location") != "https://shop.example/thanks" {
		t.Fatalf("return: %v %v", r, err)
	}
	c.eventually("USD payment succeeded", func() bool {
		return c.do("GET", "/v1/payments/"+pid, live, "", nil, 200)["status"] == "succeeded"
	})
	c.eventually("USD balance credited (1.5% fee)", func() bool {
		b := c.do("GET", "/v1/balance?currency=USD", live, "", nil, 200)
		return b["currency"] == "USD" && b["available"] == float64(4925)
	})
	if b := c.do("GET", "/v1/balance?currency=ETB", live, "", nil, 200); b["available"] != float64(0) {
		t.Fatalf("USD must not touch the ETB balance: %v", b)
	}

	// Webhook from MPGS: must carry the shared secret; duplicates are harmless.
	if code, _ := bank.Notify(c.base+"/providers/mpgs/callback", "wrong", pid); code != 401 {
		t.Fatalf("forged notification: %d", code)
	}
	if code, _ := bank.Notify(c.base+"/providers/mpgs/callback", "notify-secret", pid); code != 200 {
		t.Fatalf("notification: %d", code)
	}

	// Partial refund in USD goes to the card through MPGS.
	rf := c.do("POST", "/v1/refunds", live, "usd-r1", map[string]any{"payment_id": pid, "amount": 2000, "reason": "one item returned"}, 201)
	if rf["status"] != "succeeded" || rf["currency"] != "USD" || len(bank.Refunds) != 1 ||
		!strings.Contains(string(bank.Refunds[pid+"/"+rf["id"].(string)]), `"amount":"20.00"`) {
		t.Fatalf("refund: %v %v", rf, bank.Refunds)
	}
	c.eventually("partially refunded", func() bool {
		return c.do("GET", "/v1/payments/"+pid, live, "", nil, 200)["status"] == "partially_refunded"
	})

	// Test mode uses the bank's TEST merchant, never the live one.
	tp := c.do("POST", "/v1/payments", testKey, "usd-test", map[string]any{"amount": 1000, "currency": "USD", "payment_method": "card", "reference": "TEST-USD"}, 201)
	if !strings.Contains(tp["checkout_url"].(string), "/providers/mpgs_test/pay/") {
		t.Fatalf("test payment: %v", tp["checkout_url"])
	}
	for _, call := range bank.Calls {
		if strings.Contains(call, tp["id"].(string)) {
			t.Fatal("test payment reached the live merchant account")
		}
	}

	// A declined card fails the payment.
	d := c.do("POST", "/v1/payments", live, "usd-2", map[string]any{"amount": 7000, "currency": "USD", "payment_method": "card", "reference": "ORDER-USD-2"}, 201)
	du, _ := url.Parse(d["checkout_url"].(string))
	if r, _ := noFollow.Get(bank.Pay(du.Query().Get("session"), false)); r.StatusCode != 303 || r.Header.Get("Location") != "/pay/"+d["id"].(string) {
		t.Fatalf("declined return: %v", r.Header)
	}
	c.eventually("declined card", func() bool {
		p := c.do("GET", "/v1/payments/"+d["id"].(string), live, "", nil, 200)
		return p["status"] == "failed" && p["failure_reason"] == "declined"
	})

	// Insufficient funds: the reason reaches the merchant. The customer then
	// pays with another card in the same checkout: the capture is honoured.
	f := c.do("POST", "/v1/payments", live, "usd-3", map[string]any{"amount": 9000, "currency": "USD",
		"payment_method": "card", "reference": "ORDER-USD-3"}, 201)
	fid := f["id"].(string)
	fu, _ := url.Parse(f["checkout_url"].(string))
	sid := fu.Query().Get("session")
	bank.PayWith(sid, "INSUFFICIENT_FUNDS")
	if code, _ := bank.Notify(c.base+"/providers/mpgs/callback", "notify-secret", fid); code != 200 {
		t.Fatalf("notification: %d", code)
	}
	c.eventually("insufficient funds recorded", func() bool {
		p := c.do("GET", "/v1/payments/"+fid, live, "", nil, 200)
		return p["status"] == "failed" && p["failure_reason"] == "insufficient_funds"
	})
	if r, _ := noFollow.Get(bank.PayWith(sid, "APPROVED")); r.StatusCode != 303 {
		t.Fatalf("second card: %d", r.StatusCode)
	}
	c.eventually("paid with the second card", func() bool {
		p := c.do("GET", "/v1/payments/"+fid, live, "", nil, 200)
		return p["status"] == "succeeded" && p["failure_reason"] == nil
	})
	c.eventually("USD balance includes the second payment", func() bool {
		return c.do("GET", "/v1/balance?currency=USD", live, "", nil, 200)["available"] == float64(4925-2000+8865)
	})
}

// TestMastercardWrongCredentials: wrong API password from the bank stops
// start-up with a clear message instead of failing customers later.
func TestMastercardWrongCredentials(t *testing.T) {
	bank := mpgstest.New("DASHEN000123", "live-api-password")
	defer bank.Close()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	err := tryStart(t, func(cfg *config.Config) {
		cfg.MPGS = &config.MPGSSettings{BaseURL: bank.URL, MerchantID: bank.MerchantID, APIPassword: "wrong",
			APIVersion: "100", Currencies: []string{"USD"}, HTTP: bank.Client()}
	})
	if err == nil || !strings.Contains(err.Error(), "rejected the merchant id or API password") {
		t.Fatalf("start-up with wrong credentials: %v", err)
	}
}
