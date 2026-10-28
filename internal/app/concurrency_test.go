package app_test

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestConcurrency fires many goroutines at the same time and checks that the
// money stays correct: one payment per Idempotency-Key, no over-refund, no
// payout beyond the balance.
func TestConcurrency(t *testing.T) {
	c, _, stop := startApp(t, nil)
	defer stop()
	owner, key := c.signup("race@shop.test")

	// 1. 50 goroutines send the SAME request with the SAME Idempotency-Key.
	body := map[string]any{"amount": 7000, "currency": "ETB", "provider": "mock", "reference": "SAME"}
	res := parallel(50, func(int) rawResp { return c.raw("POST", "/v1/payments", key, "same-key", body) })
	ids, created := map[any]bool{}, 0
	for _, r := range res {
		if r.code != 201 && r.code != 200 {
			t.Fatalf("same-key request: %d %v", r.code, r.body)
		}
		if r.code == 201 {
			created++
		}
		ids[r.body["id"]] = true
	}
	if len(ids) != 1 || created != 1 {
		t.Fatalf("same Idempotency-Key from 50 goroutines: %d payments, %d created (want 1, 1)", len(ids), created)
	}

	// 2. 200 different payments from 50 goroutines at once.
	start := time.Now()
	res = parallel(200, func(i int) rawResp {
		return c.raw("POST", "/v1/payments", key, fmt.Sprintf("many-%d", i),
			map[string]any{"amount": 1000 + i, "currency": "ETB", "provider": "mock", "reference": fmt.Sprintf("M-%d", i)})
	})
	ids = map[any]bool{}
	for _, r := range res {
		if r.code != 201 {
			t.Fatalf("concurrent create: %d %v", r.code, r.body)
		}
		ids[r.body["id"]] = true
	}
	if len(ids) != 200 {
		t.Fatalf("%d distinct payments, want 200", len(ids))
	}
	t.Logf("200 concurrent payment creations in %v", time.Since(start).Round(time.Millisecond))

	// 3. 20 goroutines each refund 10% of the same payment at once:
	// exactly 10 may succeed, never more than the payment.
	pid := c.pay(key, "refund-race", 100000)
	res = parallel(20, func(i int) rawResp {
		return c.raw("POST", "/v1/refunds", key, fmt.Sprintf("rr-%d", i), map[string]any{"payment_id": pid, "amount": 10000})
	})
	ok, rejected := 0, 0
	for _, r := range res {
		switch r.code {
		case 201:
			ok++
		case 422:
			rejected++
		default:
			t.Fatalf("refund race: %d %v", r.code, r.body)
		}
	}
	if ok != 10 || rejected != 10 {
		t.Fatalf("refund race: %d succeeded, %d rejected (want 10, 10)", ok, rejected)
	}
	c.eventually("payment fully refunded, not more", func() bool {
		p := c.do("GET", "/v1/payments/"+pid, key, "", nil, 200)
		return p["status"] == "refunded" && p["refunded_amount"] == float64(100000)
	})

	// 4. 20 goroutines request payouts at once; together they ask for more
	// than the balance. Only what the balance covers may be held.
	c.pay(key, "payout-funding", 1_000_000)
	var available float64
	c.eventually("balance credited", func() bool {
		available = c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)["available"].(float64)
		return available >= 900000
	})
	bank := map[string]any{"bank_name": "Awash Bank", "account_number": "01320123456700", "account_name": "Race PLC"}
	res = parallel(20, func(i int) rawResp {
		return c.raw("POST", "/v1/payouts", owner, fmt.Sprintf("po-%d", i), map[string]any{"amount": 100000,
			"currency": "ETB", "environment": "test", "bank_account": bank})
	})
	paid := 0
	for _, r := range res {
		switch {
		case r.code == 201:
			paid++
		case r.code == 422 && r.body["error"].(map[string]any)["code"] == "insufficient_funds":
		default:
			t.Fatalf("payout race: %d %v", r.code, r.body)
		}
	}
	if want := int(available) / 100000; paid != want {
		t.Fatalf("payout race: %d payouts held with balance %.0f (want exactly %d)", paid, available, want)
	}
	bal := c.do("GET", "/v1/balance?currency=ETB", key, "", nil, 200)
	if bal["available"].(float64) < 0 || bal["pending_payouts"] != float64(paid*100000) {
		t.Fatalf("balance after payout race: %v", bal)
	}
	t.Logf("payout race: balance %.0f, %d x 100000 held, %d rejected", available, paid, 20-paid)
}

// parallel runs fn n times on n goroutines released at the same instant.
func parallel(n int, fn func(i int) rawResp) []rawResp {
	out := make([]rawResp, n)
	var ready, done sync.WaitGroup
	gate := make(chan struct{})
	ready.Add(n)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-gate // every goroutine starts together
			out[i] = fn(i)
		}(i)
	}
	ready.Wait()
	close(gate)
	done.Wait()
	return out
}
