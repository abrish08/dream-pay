package app_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"paygate/internal/modules/provider/walletmock"
	"paygate/internal/platform/config"
)

func (c *client) page(u string) string {
	c.t.Helper()
	r, err := http.Get(u)
	if err != nil {
		c.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// TestCustomerCheckout: how a customer pays from a wallet (telebirr) or by
// bank transfer. One payment never charges twice; failures are explained;
// receipts go out once.
func TestCustomerCheckout(t *testing.T) {
	wallet := walletmock.New("w-key", "w-secret")
	defer wallet.Close()
	c, _, stop := startApp(t, func(cfg *config.Config) {
		fastChecks(cfg)
		cfg.Wallets = []config.WalletSettings{{Name: "telebirr_test", Method: "telebirr", BaseURL: wallet.URL, APIKey: "w-key",
			WebhookSecret: "w-secret", Currencies: []string{"ETB"}, Sandbox: true}}
		cfg.BankAccounts = [][3]string{{"Commercial Bank of Ethiopia", "1000123456789", "Paygate PLC"}}
		cfg.BankCurrencies = []string{"ETB"}
		cfg.ProviderTestRoutes = map[string][]string{"telebirr": {"telebirr_test"}, "bank_transfer": {"bank_transfer_test"}}
	})
	defer stop()
	_, key := c.signup("customers@shop.test")
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(u string, v url.Values) (int, string, string) {
		r, err := noFollow.PostForm(u, v)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, r.Header.Get("Location"), string(b)
	}

	// 1. telebirr without the customer's phone: the checkout asks for it.
	p := c.do("POST", "/v1/payments", key, "w-1", map[string]any{"amount": 50000, "currency": "ETB", "payment_method": "telebirr",
		"reference": "W-1", "customer": map[string]string{"email": "buyer@example.com"}, "return_url": "https://shop.example/done"}, 201)
	pid, checkout := p["id"].(string), p["checkout_url"].(string)
	if p["status"] != "pending" || checkout != c.base+"/pay/"+pid || wallet.Pushes != 0 {
		t.Fatalf("phone needed first: %v (pushes %d)", p, wallet.Pushes)
	}
	if page := c.page(checkout); !strings.Contains(page, `name="phone"`) {
		t.Fatal("phone form missing")
	}
	if code, _, body := post(checkout, url.Values{"method": {"telebirr"}, "phone": {"12345"}}); code != 200 || !strings.Contains(body, "Ethiopian mobile number") {
		t.Fatalf("invalid phone: %d", code)
	}
	code, loc, _ := post(checkout, url.Values{"method": {"telebirr"}, "phone": {"0911 23 45 67"}})
	if code != 303 || loc != checkout {
		t.Fatalf("push started: %d %s", code, loc)
	}
	// Double click, refresh, a second tab: still one PIN prompt.
	post(checkout, url.Values{"method": {"telebirr"}, "phone": {"0911234567"}})
	post(checkout, url.Values{"method": {"telebirr"}, "phone": {"0922000000"}})
	if wallet.Pushes != 1 {
		t.Fatalf("one payment, %d PIN prompts", wallet.Pushes)
	}
	if push, _ := wallet.Get(pid); push.MSISDN != "+251911234567" || push.Amount != "500.00" {
		t.Fatalf("push: %+v", push)
	}
	if page := c.page(checkout); !strings.Contains(page, "Check your phone") || !strings.Contains(page, "251 9** *** 567") {
		t.Fatalf("waiting page: %s", c.page(checkout))
	}
	if page := c.page(checkout + "?lang=am"); !strings.Contains(page, "ስልክዎን ይመልከቱ") {
		t.Fatal("Amharic waiting page")
	}
	wallet.Decide(pid, "approve") // the customer enters the PIN; the wallet calls back
	c.eventually("paid", func() bool { return c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)["status"] == "succeeded" })
	if st := c.do("GET", "/pay/"+pid+"/status", "", "", nil, 200); st["status"] != "succeeded" {
		t.Fatalf("status endpoint: %v", st)
	}
	if page := c.page(checkout); !strings.Contains(page, "Payment successful") || !strings.Contains(page, "https://shop.example/done") {
		t.Fatal("success page")
	}
	c.eventually("receipts", func() bool {
		m, okMail := c.outbox.Last("buyer@example.com")
		s, okSMS := c.texts.Last("+251911234567")
		return okMail && okSMS && strings.Contains(m.Subject, "500.00 ETB paid to") && strings.Contains(s.Body, "Paid 500.00 ETB")
	})
	time.Sleep(300 * time.Millisecond)
	if n := c.outbox.Count("buyer@example.com"); n != 1 {
		t.Fatalf("receipt sent %d times", n)
	}
	rf := c.do("POST", "/v1/refunds", key, "w-1-r", map[string]any{"payment_id": pid, "amount": 10000}, 201)
	if rf["status"] != "succeeded" {
		t.Fatalf("wallet refund: %v", rf)
	}
	c.eventually("refund receipt", func() bool {
		m, _ := c.outbox.Last("buyer@example.com")
		return strings.Contains(m.Subject, "Refund: 100.00 ETB")
	})

	// 2. Insufficient funds: failed, the reason explained (in Amharic too).
	p2 := c.do("POST", "/v1/payments", key, "w-2", map[string]any{"amount": 70000, "currency": "ETB", "payment_method": "telebirr",
		"reference": "W-2", "customer": map[string]string{"phone": "0911000111"}}, 201)
	if p2["status"] != "processing" || wallet.Pushes != 2 { // phone given by the merchant: pushed at once
		t.Fatalf("direct push: %v", p2)
	}
	wallet.Decide(p2["id"].(string), "insufficient_funds")
	c.eventually("declined", func() bool {
		return c.do("GET", "/v1/payments/"+p2["id"].(string), key, "", nil, 200)["failure_reason"] == "insufficient_funds"
	})
	if page := c.page(p2["checkout_url"].(string) + "?lang=am"); !strings.Contains(page, "በቂ ገንዘብ የለም") {
		t.Fatal("Amharic failure message")
	}

	// 3. The customer never answers and the wallet sends no callback: the
	// status checks find out.
	p3 := c.do("POST", "/v1/payments", key, "w-3", map[string]any{"amount": 30000, "currency": "ETB", "payment_method": "telebirr",
		"reference": "W-3", "customer": map[string]string{"phone": "0911000222"}}, 201)
	wallet.Decide(p3["id"].(string), "timeout")
	c.eventually("timeout found by status check", func() bool {
		return c.do("GET", "/v1/payments/"+p3["id"].(string), key, "", nil, 200)["failure_reason"] == "customer_did_not_respond"
	})

	// 4. telebirr down: nothing was sent, so the next provider takes it.
	wallet.Down = true
	p4 := c.do("POST", "/v1/payments", key, "w-4", map[string]any{"amount": 30000, "currency": "ETB", "payment_method": "telebirr",
		"reference": "W-4", "customer": map[string]string{"phone": "0911000333"}}, 201)
	if p4["provider"] != "mock" {
		t.Fatalf("failover: %v", p4)
	}
	wallet.Down = false

	// 5. Bank transfer: instructions, then finance confirms from the statement.
	b := c.do("POST", "/v1/payments", key, "b-1", map[string]any{"amount": 125000, "currency": "ETB",
		"payment_method": "bank_transfer", "reference": "B-1"}, 201)
	bid := b["id"].(string)
	page := c.page(b["checkout_url"].(string))
	ref := b["provider_ref"].(string)
	if !strings.HasPrefix(ref, "PG") || !strings.Contains(page, "1000123456789") || !strings.Contains(page, ref) || !strings.Contains(page, "1250.00") {
		t.Fatalf("bank instructions (ref %s)", ref)
	}
	pending := c.do("GET", "/admin/bank-transfers?reference="+strings.ToLower(ref), c.admin, "", nil, 200)["data"].([]any)
	if len(pending) != 1 {
		t.Fatalf("finance search by reference: %v", pending)
	}
	c.do("POST", "/admin/bank-transfers/"+bid+"/confirm", c.admin, "", map[string]any{"amount": 100000, "bank_reference": "FT2611ABC"}, 422)
	c.do("POST", "/admin/bank-transfers/"+bid+"/confirm", c.admin, "", map[string]any{"amount": 125000}, 400)
	c.do("POST", "/admin/bank-transfers/"+bid+"/confirm", c.admin, "", map[string]any{"amount": 125000, "bank_reference": "FT2611ABC"}, 200)
	c.do("POST", "/admin/bank-transfers/"+bid+"/confirm", c.admin, "", map[string]any{"amount": 125000, "bank_reference": "FT2611ABC"}, 409)
	c.eventually("transfer confirmed", func() bool {
		return c.do("GET", "/v1/payments/"+bid, key, "", nil, 200)["status"] == "succeeded"
	})
	if got := trail(c.events(key, "/v1/payments/"+bid+"/events")); !strings.HasSuffix(got, "processing>succeeded:provider") {
		t.Fatalf("history: %s", got)
	}
}
