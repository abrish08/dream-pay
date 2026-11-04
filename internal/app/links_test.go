package app_test

import (
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// TestPaymentLinks: share a URL, the customer pays on the hosted page.
// A single-use link (an invoice) is paid once, even with visitors at the
// same time; a reusable link opens a new payment per visit.
func TestPaymentLinks(t *testing.T) {
	c, _, stop := startApp(t, nil)
	defer stop()
	_, key := c.signup("links@shop.test")
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	visit := func(u string) (int, string) {
		r, err := noFollow.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode, r.Header.Get("Location")
	}

	c.do("POST", "/v1/payment-links", key, "", map[string]any{"amount": -1, "currency": "ETB"}, 400)
	inv := c.do("POST", "/v1/payment-links", key, "", map[string]any{"amount": 25000, "currency": "ETB",
		"description": "Invoice 77", "reference": "INV-77", "single_use": true}, 201)
	link := inv["url"].(string)
	if !strings.HasPrefix(link, c.base+"/l/pl_") || len(inv["id"].(string)) < 20 || inv["environment"] != "test" {
		t.Fatalf("link: %v", inv)
	}

	// Ten visitors at once: one payment.
	var mu sync.Mutex
	seen := map[string]bool{}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, loc := visit(link)
			if code != 303 || !strings.Contains(loc, "/pay/") {
				t.Errorf("visit: %d %s", code, loc)
			}
			mu.Lock()
			seen[loc] = true
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(seen) != 1 {
		t.Fatalf("single-use link opened %d payments", len(seen))
	}
	var checkout string
	for k := range seen {
		checkout = k
	}
	pid := checkout[strings.LastIndex(checkout, "/")+1:]

	// The customer picks telebirr and pays.
	r, err := noFollow.PostForm(checkout, url.Values{"method": {"telebirr"}})
	if err != nil || r.StatusCode != 303 {
		t.Fatalf("choose method: %v %v", r, err)
	}
	c.checkout(r.Header.Get("Location"), true)
	c.eventually("invoice paid", func() bool {
		return c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)["status"] == "succeeded"
	})
	p := c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)
	if p["amount"] != float64(25000) || !strings.HasPrefix(p["reference"].(string), "INV-77-") ||
		p["metadata"].(map[string]any)["payment_link"] != inv["id"] {
		t.Fatalf("link payment: %v", p)
	}
	got := c.do("GET", "/v1/payment-links/"+inv["id"].(string), key, "", nil, 200)
	if got["active"] != false || got["paid_count"] != float64(1) {
		t.Fatalf("paid invoice link: %v", got)
	}
	if code, _ := visit(link); code != 410 {
		t.Fatalf("paid single-use link: %d", code)
	}

	// Reusable: each visit is a new payment; deactivated: closed.
	tag := c.do("POST", "/v1/payment-links", key, "", map[string]any{"amount": 5000, "currency": "ETB", "description": "Coffee"}, 201)
	_, a := visit(tag["url"].(string))
	_, b := visit(tag["url"].(string))
	if a == b {
		t.Fatal("reusable link reused a payment")
	}
	c.do("POST", "/v1/payment-links/"+tag["id"].(string)+"/deactivate", key, "", nil, 200)
	if code, _ := visit(tag["url"].(string)); code != 410 {
		t.Fatalf("deactivated link: %d", code)
	}
	if code, _ := visit(c.base + "/l/pl_doesnotexist"); code != 404 {
		t.Fatalf("unknown link: %d", code)
	}
	if n := len(c.do("GET", "/v1/payment-links", key, "", nil, 200)["data"].([]any)); n != 2 {
		t.Fatalf("links: %d", n)
	}
	_, other := c.signup("other-links@shop.test")
	c.do("GET", "/v1/payment-links/"+tag["id"].(string), other, "", nil, 404)
}
