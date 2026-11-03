package app_test

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"paygate/internal/platform/config"
)

func (c *client) balance(token string) map[string]any {
	return c.do("GET", "/v1/balance?currency=ETB&environment=live", token, "", nil, 200)
}

func (c *client) evidence(token, disputeID, text string, file []byte) rawResp {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("text", text)
	if file != nil {
		part, _ := w.CreateFormFile("files", "delivery-proof.pdf")
		_, _ = part.Write(file)
	}
	_ = w.Close()
	req, _ := http.NewRequest("POST", c.base+"/v1/disputes/"+disputeID+"/evidence", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := rawResp{code: resp.StatusCode, body: map[string]any{}}
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &out.body)
	return out
}

// TestReservesDisputesAndScheduledPayouts follows a merchant's money:
// rolling reserve held and released, a dispute won with evidence, one
// accepted, one lost for missing its deadline, then a scheduled payout of
// what is left. The books balance at the end.
func TestReservesDisputesAndScheduledPayouts(t *testing.T) {
	c, _, stop := startApp(t, func(cfg *config.Config) {
		cfg.AllowMockInLive = true // the sandbox stands in for a real provider
		cfg.ReserveReleaseEvery = 100 * time.Millisecond
		cfg.DisputeDeadlineEvery = 100 * time.Millisecond
		cfg.PayoutScheduleEvery = 200 * time.Millisecond
	})
	defer stop()
	owner, _ := c.signup("money@shop.test")
	mid := c.merchantID(owner)
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
	c.setSettings(mid, map[string]any{"reserve_bps": 1000, "reserve_days": 7})

	// 1. Reserve: 10% of the merchant's share is held.
	p := c.do("POST", "/v1/payments", live, "m-1", map[string]any{"amount": 100000, "currency": "ETB", "provider": "mock", "reference": "M-1"}, 201)
	if p["reserve"] != float64(9850) || p["reserve_days"] != float64(7) {
		t.Fatalf("reserve snapshot: %v", p)
	}
	c.checkout(p["checkout_url"].(string), true)
	pid := p["id"].(string)
	c.eventually("captured with reserve", func() bool {
		b := c.balance(owner)
		return b["available"] == float64(88650) && b["reserved"] == float64(9850)
	})
	// 2. Seven days later the reserve is released.
	if err := openTestDB(t).Exec("UPDATE ledger.reserve_holds SET release_at = now() - interval '1 minute' WHERE payment_id = ?", pid).Error; err != nil {
		t.Fatal(err)
	}
	c.eventually("reserve released", func() bool {
		b := c.balance(owner)
		return b["available"] == float64(98500) && b["reserved"] == float64(0)
	})

	// 3. A dispute: the amount is held at once; the merchant wins with evidence.
	c.do("POST", "/admin/disputes", c.admin, "", map[string]any{"payment_id": pid, "amount": 20000, "reason_code": "not_a_code"}, 400)
	d := c.do("POST", "/admin/disputes", c.admin, "", map[string]any{"payment_id": pid, "amount": 20000,
		"reason_code": "product_not_received", "reason": "cardholder says the order never arrived", "provider_case_id": "CB-1001"}, 201)
	did := d["id"].(string)
	if d["status"] != "needs_response" {
		t.Fatalf("dispute: %v", d)
	}
	c.eventually("disputed amount held", func() bool {
		b := c.balance(owner)
		return b["available"] == float64(78500) && b["dispute_hold"] == float64(20000)
	})
	c.do("POST", "/v1/disputes/"+did+"/submit", owner, "", nil, 400) // no evidence yet
	if r := c.evidence(owner, did, "Delivered on 2 Nov, signed by the customer.", samplePDF); r.code != 200 ||
		len(r.body["files"].([]any)) != 1 {
		t.Fatalf("evidence: %d %v", r.code, r.body)
	}
	if r := c.evidence(owner, did, "", []byte("MZ not a document")); r.code != 400 {
		t.Fatalf("executable accepted as evidence: %d", r.code)
	}
	if v := c.do("POST", "/v1/disputes/"+did+"/submit", owner, "", nil, 200); v["status"] != "under_review" {
		t.Fatalf("submit: %v", v)
	}
	c.do("POST", "/admin/disputes/"+did+"/close", c.admin, "", map[string]any{"outcome": "won", "fee": 500}, 400) // no fee when won
	if v := c.do("POST", "/admin/disputes/"+did+"/close", c.admin, "", map[string]any{"outcome": "won", "note": "issuer accepted the proof"}, 200); v["status"] != "won" {
		t.Fatalf("won: %v", v)
	}
	c.eventually("money back after winning", func() bool {
		b := c.balance(owner)
		return b["available"] == float64(98500) && b["dispute_hold"] == float64(0)
	})

	// 4. A dispute the merchant accepts, with the provider's fee.
	d2 := c.do("POST", "/admin/disputes", c.admin, "", map[string]any{"payment_id": pid, "amount": 10000, "reason_code": "duplicate"}, 201)
	c.do("POST", "/v1/disputes/"+d2["id"].(string)+"/accept", owner, "", nil, 200)
	c.eventually("accepted dispute lost", func() bool { return c.balance(owner)["available"] == float64(88500) })
	// More than is left of the payment cannot be disputed.
	c.do("POST", "/admin/disputes", c.admin, "", map[string]any{"payment_id": pid, "amount": 90001, "reason_code": "fraudulent"}, 422)

	// 5. A dispute without evidence by its deadline is lost automatically.
	d3 := c.do("POST", "/admin/disputes", c.admin, "", map[string]any{"payment_id": pid, "amount": 5000, "reason_code": "fraudulent",
		"evidence_due_by": time.Now().Add(1500 * time.Millisecond).UTC()}, 201)
	c.eventually("overdue dispute lost", func() bool {
		return c.do("GET", "/v1/disputes/"+d3["id"].(string), owner, "", nil, 200)["status"] == "lost"
	})
	c.eventually("lost amount gone", func() bool {
		b := c.balance(owner)
		return b["available"] == float64(83500) && b["dispute_hold"] == float64(0)
	})
	if n := len(c.do("GET", "/v1/disputes", owner, "", nil, 200)["data"].([]any)); n != 3 {
		t.Fatalf("merchant's disputes: %d", n)
	}
	got := trail(c.events(live, "/v1/payments/"+pid+"/events"))
	if strings.Count(got, ":dispute") != 6 { // 3 opened + 3 closed
		t.Fatalf("disputes in the payment history: %s", got)
	}

	// 6. Daily scheduled payout of the available balance to the verified bank account.
	c.do("PATCH", "/admin/merchants/"+mid, c.admin, "", map[string]any{"settings": map[string]any{
		"reserve_bps": 1000, "reserve_days": 7, "payout": map[string]any{"schedule": "daily"}}}, 400) // no bank account
	c.setSettings(mid, map[string]any{"reserve_bps": 1000, "reserve_days": 7, "payout": map[string]any{"schedule": "daily",
		"bank": map[string]string{"bank_name": "CBE", "account_number": "1000123456789", "account_name": "Money Shop PLC"}}})
	var payouts []any
	c.eventually("scheduled payout created", func() bool {
		payouts = c.do("GET", "/v1/payouts", owner, "", nil, 200)["data"].([]any)
		return len(payouts) == 1
	})
	po := payouts[0].(map[string]any)
	if po["amount"] != float64(83500) || po["status"] != "requested" || po["environment"] != "live" {
		t.Fatalf("scheduled payout: %v", po)
	}
	time.Sleep(time.Second) // more scheduler runs: still one payout today
	if n := len(c.do("GET", "/v1/payouts", owner, "", nil, 200)["data"].([]any)); n != 1 {
		t.Fatalf("payouts today: %d", n)
	}
	if b := c.balance(owner); b["available"] != float64(0) || b["pending_payouts"] != float64(83500) {
		t.Fatalf("after payout: %v", b)
	}

	// The books balance.
	for _, row := range c.do("GET", "/admin/ledger/trial-balance", c.admin, "", nil, 200)["data"].([]any) {
		if row.(map[string]any)["balanced"] != true {
			t.Fatalf("ledger unbalanced: %v", row)
		}
	}
}
