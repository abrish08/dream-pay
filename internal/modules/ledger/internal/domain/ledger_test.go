package domain

import (
	"testing"

	"paygate/internal/contracts/disputes"
	"paygate/internal/contracts/payments"
	"paygate/internal/contracts/refunds"
)

func TestPostingsBalance(t *testing.T) {
	captured := PaymentCaptured(payments.PaymentSucceeded{
		PaymentID: "p1", MerchantID: "m1", Environment: "live", Amount: 150_000, Fee: 2_250,
		Currency: "ETB", Provider: "telebirr",
	})
	if err := captured.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(captured.Lines) != 3 {
		t.Fatalf("want 3 lines (clearing, merchant, fees), got %d", len(captured.Lines))
	}

	zeroFee := PaymentCaptured(payments.PaymentSucceeded{PaymentID: "p2", MerchantID: "m1", Environment: "live",
		Amount: 100, Currency: "ETB", Provider: "telebirr"})
	if err := zeroFee.Validate(); err != nil || len(zeroFee.Lines) != 2 {
		t.Fatalf("zero fee posting: %v, lines %d", err, len(zeroFee.Lines))
	}

	refund := RefundSucceeded(refunds.RefundSucceeded{RefundID: "r1", PaymentID: "p1", MerchantID: "m1",
		Environment: "live", Amount: 30_000, Currency: "ETB", Provider: "telebirr"})
	if err := refund.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUnbalancedRejected(t *testing.T) {
	p := Posting{Reference: "x", Currency: "ETB", Lines: []Line{
		{Account: "a", Debit: 100}, {Account: "b", Credit: 99},
	}}
	if p.Validate() == nil {
		t.Fatal("unbalanced posting must be rejected")
	}
	both := Posting{Reference: "x", Currency: "ETB", Lines: []Line{
		{Account: "a", Debit: 100, Credit: 100}, {Account: "b", Credit: 0, Debit: 0},
	}}
	if both.Validate() == nil {
		t.Fatal("lines must have exactly one side")
	}
}

func TestTestAndLiveMoneyNeverMix(t *testing.T) {
	if MerchantAvailable("m1", "test") == MerchantAvailable("m1", "live") {
		t.Fatal("test and live balances must use different accounts")
	}
}

func TestReserveAndDisputePostingsBalance(t *testing.T) {
	captured := PaymentCaptured(payments.PaymentSucceeded{PaymentID: "p", MerchantID: "m", Environment: "live", Amount: 100000,
		Fee: 1500, Reserve: 9850, ReserveDays: 7, Currency: "ETB", Provider: "telebirr"})
	if err := captured.Validate(); err != nil {
		t.Fatal(err)
	}
	var reserved, available int64
	for _, l := range captured.Lines {
		switch l.Account {
		case MerchantReserve("m", "live"):
			reserved += l.Credit
		case MerchantAvailable("m", "live"):
			available += l.Credit
		}
	}
	if reserved != 9850 || available != 88650 {
		t.Fatalf("reserve split: reserved %d available %d", reserved, available)
	}
	for _, p := range []Posting{
		ReserveReleased("p", "m", "live", "ETB", 9850),
		DisputeOpened(disputes.DisputeOpened{DisputeID: "d", PaymentID: "p", MerchantID: "m", Environment: "live", Amount: 20000, Currency: "ETB"}),
		DisputeClosed(disputes.DisputeClosed{DisputeID: "d", MerchantID: "m", Environment: "live", Amount: 20000, Currency: "ETB", Outcome: "won"}),
		DisputeClosed(disputes.DisputeClosed{DisputeID: "d2", MerchantID: "m", Environment: "live", Amount: 20000, Currency: "ETB",
			Provider: "telebirr", Outcome: "lost", Fee: 1500}),
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%s: %v", p.Reference, err)
		}
	}
}
