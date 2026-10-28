package domain

import (
	"errors"
	"testing"
	"time"
)

func newPayment(t *testing.T) *Payment {
	t.Helper()
	p, err := NewPayment(NewPaymentInput{
		MerchantID: "m1", Environment: "test", Amount: 150_000, Currency: "etb",
		Provider: "mock", Reference: "INV-1", TTL: 30 * time.Minute,
		Metadata: map[string]string{"order_id": "42"},
	}, 150, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFeeIsSnapshottedAndRounded(t *testing.T) {
	if got := CalculateFee(150_000, 150); got != 2_250 {
		t.Fatalf("fee = %d, want 2250", got)
	}
	if got := CalculateFee(333, 150); got != 5 { // 4.995 rounds half up
		t.Fatalf("fee = %d, want 5", got)
	}
	if p := newPayment(t); p.Fee != 2_250 || p.Currency != "ETB" {
		t.Fatalf("payment fee/currency = %d/%s", p.Fee, p.Currency)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]NewPaymentInput{
		"zero amount":  {Amount: 0, Currency: "ETB", Provider: "mock", Reference: "x"},
		"bad currency": {Amount: 1, Currency: "EUR", Provider: "mock", Reference: "x"},
		"no reference": {Amount: 1, Currency: "ETB", Provider: "mock"},
		"bad email":    {Amount: 1, Currency: "ETB", Provider: "mock", Reference: "x", CustomerEmail: "nope"},
		"ttl too long": {Amount: 1, Currency: "ETB", Provider: "mock", Reference: "x", TTL: 48 * time.Hour},
		"bad url":      {Amount: 1, Currency: "ETB", Provider: "mock", Reference: "x", ReturnURL: "javascript:x"},
		"long meta": {Amount: 1, Currency: "ETB", Provider: "mock", Reference: "x",
			Metadata: map[string]string{"k": string(make([]byte, 501))}},
	}
	for name, in := range cases {
		if in.TTL == 0 {
			in.TTL = 30 * time.Minute
		}
		if _, err := NewPayment(in, 0, time.Now()); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestStateMachine(t *testing.T) {
	now := time.Now()
	p := newPayment(t)
	if err := p.TransitionTo(Succeeded, now); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pending -> succeeded must be rejected, got %v", err)
	}
	if err := p.StartProcessing("ref", "url", now); err != nil {
		t.Fatal(err)
	}
	if err := p.TransitionTo(Succeeded, now); err != nil {
		t.Fatal(err)
	}
	if err := p.Fail("late", now); err == nil {
		t.Fatal("succeeded -> failed must be rejected")
	}
	if !p.Captured() {
		t.Fatal("succeeded payment should be captured")
	}
}

func TestRefunds(t *testing.T) {
	now := time.Now()
	p := newPayment(t)
	_ = p.StartProcessing("ref", "url", now)
	_ = p.TransitionTo(Succeeded, now)

	if err := p.ApplyRefund(50_000, now); err != nil || p.Status != PartiallyRefunded {
		t.Fatalf("partial refund: %v, status %s", err, p.Status)
	}
	if err := p.ApplyRefund(200_000, now); err == nil {
		t.Fatal("over-refund must be rejected")
	}
	if err := p.ApplyRefund(100_000, now); err != nil || p.Status != Refunded || p.RefundedAmount != 150_000 {
		t.Fatalf("final refund: %v, status %s, refunded %d", err, p.Status, p.RefundedAmount)
	}
}

func TestExpiry(t *testing.T) {
	now := time.Now()
	p := newPayment(t)
	if !p.ExpiresAt.Equal(p.CreatedAt.Add(30 * time.Minute)) {
		t.Fatal("expires_at not set from TTL")
	}
	_ = p.StartProcessing("ref", "url", now)
	if err := p.Expire(now); err != nil || p.Status != Expired || p.Captured() {
		t.Fatalf("expire: %v %s", err, p.Status)
	}
	// The customer paid late and the provider confirmed it: honour it.
	if err := p.TransitionTo(Succeeded, now); err != nil {
		t.Fatalf("late success must be accepted: %v", err)
	}
	if err := p.Expire(now); err == nil {
		t.Fatal("succeeded payment cannot expire")
	}
}
