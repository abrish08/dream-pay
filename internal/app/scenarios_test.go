package app_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"paygate/internal/platform/config"
)

// TestScenarios runs the gateway through real business stories, end to end,
// on ONE running instance (like production): every story crosses several
// services and checks money, events and permissions at each step.
//
//	TEST_DATABASE_URL=postgres://…/paygate_test?sslmode=disable go test ./internal/app -run TestScenarios -v
func TestScenarios(t *testing.T) {
	c, _, stop := startApp(t, func(cfg *config.Config) {
		cfg.AllowMockInLive = true // the sandbox stands in for a real provider in live mode
	})
	defer stop()

	scenario := func(name string, fn func(s *story)) {
		t.Run(name, func(t *testing.T) {
			sc := *c
			sc.t = t
			fn(&story{t: t, c: &sc})
		})
	}

	scenario("S01 merchant signs up, verifies email, sells in test mode, passes KYC and goes live", func(s *story) {
		owner, key := s.c.signup("s01@abebecoffee.test")
		s.step("signed up and confirmed the 6-digit email code: logged in, test API key created")
		m := s.c.do("GET", "/v1/merchant", owner, "", nil, 200)
		s.expect(m["status"] == "pending" && m["kyc_status"] == "not_started", "new merchant is pending, KYC not started: %v", m)
		s.c.pay(key, "s01-test", 25000)
		s.step("test payment of 250.00 ETB succeeded without KYC")
		e := s.c.do("POST", "/v1/api-keys", owner, "", map[string]any{"name": "live", "environment": "live", "scopes": []string{"payments:write"}}, 403)
		s.expect(code(e) == "kyc_required", "live key refused before KYC: %v", e)
		s.step("live API key refused: kyc_required")
		s.c.passKYC(owner, m["id"].(string))
		s.step("filled the business profile, uploaded 6 documents, submitted; staff approved; merchant became active")
		var live string
		s.c.eventually("live key after KYC", func() bool {
			r := s.c.raw("POST", "/v1/api-keys", owner, "", map[string]any{"name": "live", "environment": "live",
				"scopes": []string{"payments:read", "payments:write", "balance:read"}})
			if r.code == 201 {
				live = r.body["secret"].(string)
			}
			return r.code == 201
		})
		s.expect(strings.HasPrefix(live, "sk_live_"), "live key format")
		s.step("live API key issued")
		p := s.c.do("POST", "/v1/payments", live, "s01-live", map[string]any{"amount": 100000, "currency": "ETB",
			"payment_method": "telebirr", "reference": "LIVE-1"}, 201)
		s.c.checkout(p["checkout_url"].(string), true)
		s.c.eventually("live payment succeeded", func() bool {
			return s.c.do("GET", "/v1/payments/"+p["id"].(string), live, "", nil, 200)["status"] == "succeeded"
		})
		s.c.eventually("live balance credited (ledger posts asynchronously)", func() bool {
			bal := s.c.do("GET", "/v1/balance?currency=ETB", live, "", nil, 200)
			return bal["environment"] == "live" && bal["available"] == float64(98500)
		})
		s.step("first live payment of 1,000.00 ETB succeeded; live balance 985.00 ETB (1.5%% fee)")
	})

	scenario("S02 customer pays an order with telebirr; merchant is notified by signed webhook", func(s *story) {
		owner, key := s.c.signup("s02@shop.test")
		_ = owner
		hooks := s.webhooks(key)
		p := s.c.do("POST", "/v1/payments", key, "s02-order-1001", map[string]any{"amount": 150000, "currency": "ETB",
			"payment_method": "telebirr", "reference": "ORDER-1001", "description": "2 x Yirgacheffe 500g",
			"metadata": map[string]string{"order_id": "1001"}, "customer": map[string]string{"email": "hana@example.com", "name": "Hana"},
			"return_url": "https://shop.example/orders/1001"}, 201)
		s.expect(p["status"] == "processing" && p["payment_method"] == "telebirr", "payment opened: %v", p)
		s.step("merchant server created the payment; customer sent to %s", p["checkout_url"])
		s.c.checkout(p["checkout_url"].(string), true)
		s.step("customer paid on the provider page and was sent back to the shop")
		s.c.eventually("payment.succeeded webhook", func() bool { return hooks.types()["payment.succeeded"] == 1 })
		ev := hooks.last("payment.succeeded")
		data := ev["data"].(map[string]any)
		s.expect(data["reference"] == "ORDER-1001" && data["metadata"].(map[string]any)["order_id"] == "1001",
			"webhook carries the order: %v", data)
		s.expect(hooks.badSignatures() == 0, "webhook signature valid")
		s.step("webhook payment.succeeded arrived, signature valid, carries order_id 1001")
		s.expect(s.c.do("GET", "/v1/payments/"+p["id"].(string), key, "", nil, 200)["status"] == "succeeded", "status succeeded")
		found := s.c.do("GET", "/v1/payments?reference=ORDER-1001", key, "", nil, 200)["data"].([]any)
		s.expect(len(found) == 1, "order findable by reference")
		s.c.eventually("balance credited 1,500 - 22.50 fee", func() bool {
			return s.c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"] == float64(147750)
		})
		s.step("payment found by its order reference; balance credited 1,477.50 ETB")
	})

	scenario("S03 customer chooses how to pay on the hosted checkout page", func(s *story) {
		_, key := s.c.signup("s03@shop.test")
		p := s.c.do("POST", "/v1/payments", key, "s03", map[string]any{"amount": 45000, "currency": "ETB", "reference": "H-1"}, 201)
		s.expect(strings.HasSuffix(p["checkout_url"].(string), "/pay/"+p["id"].(string)), "hosted page link")
		s.step("payment created without a method: checkout link is the hosted page")
		page := s.get(p["checkout_url"].(string))
		s.expect(strings.Contains(page, "CBE Birr") && strings.Contains(page, "telebirr") && strings.Contains(page, "450.00"),
			"page lists the methods and the amount")
		s.step("hosted page shows 450.00 ETB and the available methods")
		next := s.postForm(p["checkout_url"].(string), url.Values{"method": {"cbe_birr"}})
		s.c.checkout(next, true)
		s.c.eventually("paid with CBE Birr", func() bool {
			got := s.c.do("GET", "/v1/payments/"+p["id"].(string), key, "", nil, 200)
			return got["status"] == "succeeded" && got["payment_method"] == "cbe_birr"
		})
		s.step("customer chose CBE Birr, was routed to the provider and paid")
	})

	scenario("S04 customer declines: payment fails, nothing is credited", func(s *story) {
		_, key := s.c.signup("s04@shop.test")
		hooks := s.webhooks(key)
		p := s.c.do("POST", "/v1/payments", key, "s04", map[string]any{"amount": 30000, "currency": "ETB",
			"payment_method": "mpesa", "reference": "D-1"}, 201)
		s.c.checkout(p["checkout_url"].(string), false)
		s.c.eventually("payment failed", func() bool {
			return s.c.do("GET", "/v1/payments/"+p["id"].(string), key, "", nil, 200)["status"] == "failed"
		})
		s.c.eventually("payment.failed webhook", func() bool { return hooks.types()["payment.failed"] == 1 })
		bal := s.c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)
		s.expect(bal["available"] == float64(0), "nothing credited: %v", bal)
		s.c.do("POST", "/v1/refunds", key, "s04-r", map[string]any{"payment_id": p["id"]}, 422)
		s.step("decline recorded, payment.failed sent, balance unchanged, refund impossible")
	})

	scenario("S05 abandoned checkout expires; a late payment is still honoured", func(s *story) {
		_, key := s.c.signup("s05@shop.test")
		hooks := s.webhooks(key)
		p := s.c.do("POST", "/v1/payments", key, "s05", map[string]any{"amount": 5000, "currency": "ETB",
			"payment_method": "telebirr", "reference": "A-1", "expires_in_minutes": 5}, 201)
		s.c.expireNow(p["id"].(string))
		s.c.eventually("payment expired", func() bool {
			return s.c.do("GET", "/v1/payments/"+p["id"].(string), key, "", nil, 200)["status"] == "expired"
		})
		s.c.eventually("payment.expired webhook", func() bool { return hooks.types()["payment.expired"] == 1 })
		s.step("customer left; after the deadline the provider was asked, then the payment expired (webhook sent)")
		s.c.checkout(p["checkout_url"].(string), true)
		s.c.eventually("late payment honoured", func() bool {
			return s.c.do("GET", "/v1/payments/"+p["id"].(string), key, "", nil, 200)["status"] == "succeeded" &&
				hooks.types()["payment.succeeded"] == 1
		})
		s.step("customer paid late at the provider: payment moved to succeeded, payment.succeeded sent")
	})

	scenario("S06 refunds: partial, provider decline, over-refund, then the rest", func(s *story) {
		_, key := s.c.signup("s06@shop.test")
		hooks := s.webhooks(key)
		pid := s.c.pay(key, "s06", 100000)
		s.step("customer paid 1,000.00 ETB")
		r := s.c.do("POST", "/v1/refunds", key, "s06-r1", map[string]any{"payment_id": pid, "amount": 30000, "reason": "one item returned"}, 201)
		s.expect(r["status"] == "succeeded", "partial refund: %v", r)
		s.c.eventually("partially refunded", func() bool {
			return s.c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)["status"] == "partially_refunded"
		})
		s.step("refunded 300.00 ETB: payment partially_refunded")
		d := s.c.do("POST", "/v1/refunds", key, "s06-r2", map[string]any{"payment_id": pid, "amount": 113}, 201)
		s.expect(d["status"] == "failed", "provider declined the refund: %v", d)
		s.c.do("POST", "/v1/refunds", key, "s06-r3", map[string]any{"payment_id": pid, "amount": 999999}, 422)
		s.step("provider declined one refund (recorded as failed); refunding more than paid was refused")
		all := s.c.do("POST", "/v1/refunds", key, "s06-r4", map[string]any{"payment_id": pid}, 201)
		s.expect(all["amount"] == float64(70000), "amount omitted = refund the rest (700.00): %v", all)
		s.c.eventually("fully refunded", func() bool {
			p := s.c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)
			return p["status"] == "refunded" && p["refunded_amount"] == float64(100000)
		})
		s.c.eventually("refund webhooks", func() bool {
			ty := hooks.types()
			return ty["refund.succeeded"] == 2 && ty["refund.failed"] == 1
		})
		listed := s.c.do("GET", "/v1/refunds?payment_id="+pid, key, "", nil, 200)["data"].([]any)
		s.expect(len(listed) == 3, "three refunds recorded (2 ok, 1 declined)")
		s.c.eventually("balance after refunds", func() bool {
			return s.c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"] == float64(-1500)
		})
		s.step("refunded the remaining 700.00: payment refunded; balance -15.00 ETB (the platform fee is not returned)")
	})

	scenario("S07 settlement: payout to the bank, a failed transfer, books stay balanced", func(s *story) {
		owner, key := s.c.signup("s07@shop.test")
		hooks := s.webhooks(key)
		s.c.pay(key, "s07", 500000)
		s.c.eventually("balance", func() bool {
			return s.c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"] == float64(492500)
		})
		s.step("earned 4,925.00 ETB (5,000 - 1.5%% fee)")
		bank := map[string]any{"bank_name": "Commercial Bank of Ethiopia", "account_number": "1000123456789", "account_name": "Shop PLC"}
		e := s.c.do("POST", "/v1/payouts", owner, "s07-p0", map[string]any{"amount": 600000, "currency": "ETB", "environment": "test", "bank_account": bank}, 422)
		s.expect(code(e) == "insufficient_funds", "too large: %v", e)
		s.c.do("POST", "/v1/payouts", key, "s07-pk", map[string]any{"amount": 10000, "currency": "ETB", "bank_account": bank}, 403)
		s.step("payout above the balance refused; payouts never possible with an API key")
		p1 := s.c.do("POST", "/v1/payouts", owner, "s07-p1", map[string]any{"amount": 200000, "currency": "ETB", "environment": "test", "bank_account": bank}, 201)
		bal := s.c.do("GET", "/v1/balance?currency=ETB&environment=test", owner, "", nil, 200)
		s.expect(bal["available"] == float64(292500) && bal["pending_payouts"] == float64(200000), "held: %v", bal)
		s.step("requested 2,000.00 ETB: funds held at once (available 2,925.00, pending 2,000.00)")
		s.c.do("GET", "/admin/payouts?status=requested", s.c.admin, "", nil, 200)
		s.c.do("POST", "/admin/payouts/"+p1["id"].(string)+"/paid", s.c.admin, "", map[string]any{"bank_reference": "CBE-FT-1"}, 200)
		p2 := s.c.do("POST", "/v1/payouts", owner, "s07-p2", map[string]any{"amount": 50000, "currency": "ETB", "environment": "test", "bank_account": bank}, 201)
		s.c.do("POST", "/admin/payouts/"+p2["id"].(string)+"/failed", s.c.admin, "", map[string]any{"reason": "account closed"}, 200)
		s.step("finance sent the first transfer (paid) and marked a second as failed")
		s.c.eventually("ledger settled", func() bool {
			b := s.c.do("GET", "/v1/balance?currency=ETB&environment=test", owner, "", nil, 200)
			return b["available"] == float64(292500) && b["pending_payouts"] == float64(0)
		})
		s.c.eventually("payout webhooks", func() bool {
			ty := hooks.types()
			return ty["payout.paid"] == 1 && ty["payout.failed"] == 1
		})
		s.step("paid payout booked, failed payout released back; payout.paid and payout.failed webhooks sent")
	})

	scenario("S08 network retries never double-charge", func(s *story) {
		_, key := s.c.signup("s08@shop.test")
		body := map[string]any{"amount": 12000, "currency": "ETB", "payment_method": "telebirr", "reference": "R-1"}
		first := s.c.raw("POST", "/v1/payments", key, "s08-retry", body)
		again := s.c.raw("POST", "/v1/payments", key, "s08-retry", body)
		s.expect(first.code == 201 && again.code == 200 && first.body["id"] == again.body["id"] &&
			again.header.Get("Idempotent-Replayed") == "true", "replay returns the same payment")
		s.step("the same request sent twice (lost response, retry): one payment, the second answer is a replay")
		body["amount"] = 99999
		s.c.do("POST", "/v1/payments", key, "s08-retry", body, 409)
		s.step("reusing the key for a different request is refused (409)")
		n := len(s.c.do("GET", "/v1/payments?reference=R-1", key, "", nil, 200)["data"].([]any))
		s.expect(n == 1, "exactly one payment exists: %d", n)
	})

	scenario("S09 merchant invites a developer with limited access", func(s *story) {
		owner, _ := s.c.signup("s09@shop.test")
		s.c.do("POST", "/v1/team/members", owner, "", map[string]any{"email": "dev@s09.test", "name": "Dawit",
			"password": "dev-password-12345", "roles": []string{"developer"}}, 201)
		dev := s.c.loginVerified("dev@s09.test", "dev-password-12345")["access_token"].(string)
		s.step("owner invited a developer; the developer verified their email on first login")
		s.c.do("POST", "/v1/api-keys", dev, "", map[string]any{"name": "ci", "environment": "test", "scopes": []string{"payments:read", "payments:write"}}, 201)
		s.c.do("POST", "/v1/api-keys", dev, "", map[string]any{"name": "x", "environment": "test", "scopes": []string{"refunds:write"}}, 403)
		s.c.do("POST", "/v1/team/members", dev, "", map[string]any{"email": "x@s09.test", "name": "X", "password": "x-password-123456", "roles": []string{"viewer"}}, 403)
		s.c.do("GET", "/admin/merchants", dev, "", nil, 403)
		s.step("developer can create payment keys, but cannot grant refunds, invite people or reach staff pages")
	})

	scenario("S10 webhook endpoint is down, then recovers", func(s *story) {
		_, key := s.c.signup("s10@shop.test")
		hooks := s.webhooks(key)
		hooks.setFailing(true)
		s.c.pay(key, "s10", 20000)
		var delivery string
		s.c.eventually("failed delivery recorded", func() bool {
			for _, d := range s.c.do("GET", "/v1/webhook-deliveries", key, "", nil, 200)["data"].([]any) {
				d := d.(map[string]any)
				if d["event_type"] == "payment.succeeded" && d["last_status_code"] == float64(500) && d["status"] == "pending" {
					delivery = d["id"].(string)
				}
			}
			return delivery != ""
		})
		s.step("merchant endpoint answered 500: delivery kept pending for automatic retry (backoff)")
		hooks.setFailing(false)
		s.c.do("POST", "/v1/webhook-deliveries/"+delivery+"/retry", key, "", nil, 202)
		s.c.eventually("delivered after retry", func() bool { return hooks.types()["payment.succeeded"] == 1 })
		s.step("endpoint fixed; merchant pressed retry; the event was delivered")
	})

	scenario("S11 forgot password: reset by email ends every old session", func(s *story) {
		owner, _ := s.c.signup("s11@shop.test")
		s.c.do("POST", "/auth/password/forgot", "", "", map[string]any{"email": "s11@shop.test"}, 202)
		var token string
		s.c.eventually("reset email", func() bool {
			m, _ := s.c.outbox.Last("s11@shop.test")
			if i := strings.Index(m.Text, "token="); i >= 0 {
				token = strings.Fields(m.Text[i+6:])[0]
			}
			return token != ""
		})
		s.step("asked for a reset; the link arrived by email")
		s.c.do("POST", "/auth/password/reset", "", "", map[string]any{"token": token, "new_password": "brand-new-password-1"}, 204)
		s.c.eventually("old session ended", func() bool { return s.c.status2("GET", "/auth/me", owner, nil) == 401 })
		s.c.login("s11@shop.test", "owner-password-123", 401)
		s.c.login("s11@shop.test", "brand-new-password-1", 200)
		s.c.do("POST", "/auth/password/reset", "", "", map[string]any{"token": token, "new_password": "another-password-12"}, 400)
		s.step("new password set; old token and old password rejected; the link cannot be used twice")
	})

	scenario("S12 suspension blocks a merchant everywhere; reactivation restores it; no KYC bypass", func(s *story) {
		owner, key := s.c.signup("s12@shop.test")
		mid := s.c.do("GET", "/v1/merchant", owner, "", nil, 200)["id"].(string)
		s.c.passKYC(owner, mid)
		s.step("verified merchant is active")
		s.c.do("POST", "/admin/merchants/"+mid+"/status", s.c.admin, "", map[string]any{"status": "suspended"}, 200)
		s.c.eventually("API key blocked", func() bool { return s.c.status2("GET", "/v1/payments", key, nil) == 403 })
		s.c.login("s12@shop.test", "owner-password-123", 403)
		s.step("staff suspended it: its API key and logins stop working")
		s.c.do("POST", "/admin/merchants/"+mid+"/status", s.c.admin, "", map[string]any{"status": "active"}, 200)
		s.c.eventually("API key works again", func() bool { return s.c.status2("GET", "/v1/payments", key, nil) == 200 })
		s.c.login("s12@shop.test", "owner-password-123", 200)
		s.step("staff reactivated it: API key and login work again")

		other, _ := s.c.signup("s12-unverified@shop.test")
		oid := s.c.do("GET", "/v1/merchant", other, "", nil, 200)["id"].(string)
		s.c.do("POST", "/admin/merchants/"+oid+"/status", s.c.admin, "", map[string]any{"status": "suspended"}, 200)
		e := s.c.do("POST", "/admin/merchants/"+oid+"/status", s.c.admin, "", map[string]any{"status": "active"}, 409)
		s.expect(code(e) == "kyc_not_approved", "suspend-then-activate must not skip KYC: %v", e)
		s.step("a never-verified merchant cannot be activated through suspend → activate")
	})

	scenario("S13 staff change what every merchant may do", func(s *story) {
		owner, _ := s.c.signup("s13@shop.test")
		var roleID string
		for _, r := range s.c.do("GET", "/admin/roles?audience=merchant", s.c.admin, "", nil, 200)["data"].([]any) {
			if r.(map[string]any)["name"] == "merchant" {
				roleID = r.(map[string]any)["id"].(string)
			}
		}
		s.c.do("DELETE", "/admin/roles/"+roleID+"/permissions/team:manage", s.c.admin, "", nil, 200)
		s.c.eventually("token stale", func() bool { return s.c.status2("GET", "/auth/me", owner, nil) == 401 })
		owner = s.c.login("s13@shop.test", "owner-password-123", 200)["access_token"].(string)
		s.c.do("GET", "/v1/team/members", owner, "", nil, 403)
		s.step("staff removed team:manage from the merchant role: every merchant lost it within seconds")
		s.c.do("POST", "/admin/roles/"+roleID+"/permissions", s.c.admin, "", map[string]any{"permissions": []string{"team:manage"}}, 200)
		s.c.eventually("token stale again", func() bool { return s.c.status2("GET", "/auth/me", owner, nil) == 401 })
		owner = s.c.login("s13@shop.test", "owner-password-123", 200)["access_token"].(string)
		s.c.do("GET", "/v1/team/members", owner, "", nil, 200)
		s.step("staff added it back: merchants regained it")
	})

	scenario("S14 daily operations: reconciliation and the books", func(s *story) {
		run := s.c.do("POST", "/admin/reconciliation/runs", s.c.admin, "", map[string]any{
			"from": "2026-01-01T00:00:00Z", "to": "2026-01-31T00:00:00Z"}, 202)
		s.c.eventually("reconciliation finished", func() bool {
			return s.c.do("GET", "/admin/reconciliation/runs/"+run["id"].(string), s.c.admin, "", nil, 200)["status"] == "completed"
		})
		s.step("reconciliation run completed")
		for _, row := range s.c.do("GET", "/admin/ledger/trial-balance", s.c.admin, "", nil, 200)["data"].([]any) {
			r := row.(map[string]any)
			s.expect(r["balanced"] == true, "ledger unbalanced: %v", r)
		}
		s.step("after every story above, debits equal credits in the ledger")
	})
}

// story narrates and checks one scenario.
type story struct {
	t *testing.T
	c *client
	n int
}

func (s *story) step(msg string, args ...any) {
	s.n++
	s.t.Logf("  %2d. %s", s.n, fmt.Sprintf(msg, args...))
}

func (s *story) expect(ok bool, msg string, args ...any) {
	s.t.Helper()
	if !ok {
		s.t.Fatalf("expected: "+msg, args...)
	}
}

// webhooks registers a fresh receiving endpoint for the merchant of key.
func (s *story) webhooks(key string) *receiver {
	r := &receiver{}
	srv := httptest.NewServer(r)
	s.t.Cleanup(srv.Close)
	ep := s.c.do("POST", "/v1/webhook-endpoints", key, "", map[string]any{"url": srv.URL, "events": []string{"*"}}, 201)
	r.setSecret(ep["secret"].(string))
	return r
}

func (s *story) get(u string) string {
	resp, err := http.Get(u)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}

// postForm submits a form without following the redirect; returns Location.
func (s *story) postForm(u string, v url.Values) string {
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.PostForm(u, v)
	if err != nil {
		s.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		s.t.Fatalf("form %s: status %d", u, resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

func code(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}
