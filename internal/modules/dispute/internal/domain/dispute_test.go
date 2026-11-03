package domain

import (
	"testing"
	"time"

	"paygate/internal/contracts/disputes"
)

func open(t *testing.T, now time.Time) *Dispute {
	d, err := Open(OpenInput{PaymentID: "p", MerchantID: "m", Amount: 5000, Disputable: 10000, ReasonCode: "fraudulent",
		EvidenceDueBy: now.Add(time.Hour)}, now)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDisputeLifecycle(t *testing.T) {
	now := time.Now()
	if _, err := Open(OpenInput{Amount: 20000, Disputable: 10000, ReasonCode: "fraudulent", EvidenceDueBy: now.Add(time.Hour)}, now); err == nil {
		t.Fatal("more than the payment")
	}
	d := open(t, now)
	if err := d.Submit(0, now); err == nil {
		t.Fatal("submitted without evidence")
	}
	if err := d.Close(disputes.OutcomeWon, 0, "", "staff", now); err == nil {
		t.Fatal("won without review")
	}
	_ = d.AddEvidence("tracking number ET123", now)
	if err := d.Submit(0, now); err != nil || d.Status != disputes.StatusUnderReview {
		t.Fatalf("submit: %v", err)
	}
	if err := d.AddEvidence("more", now); err == nil {
		t.Fatal("evidence after submitting")
	}
	if err := d.Close(disputes.OutcomeWon, 100, "", "staff", now); err == nil {
		t.Fatal("fee on a won dispute")
	}
	if err := d.Close(disputes.OutcomeWon, 0, "ok", "staff", now); err != nil || d.Status != disputes.StatusWon {
		t.Fatalf("won: %v", err)
	}
	if err := d.Close(disputes.OutcomeLost, 0, "", "staff", now); err == nil {
		t.Fatal("decided twice")
	}
	late := open(t, now)
	if err := late.AddEvidence("x", now.Add(2*time.Hour)); err == nil {
		t.Fatal("evidence after the deadline")
	}
}
