// Package domain: a dispute (chargeback) is a cardholder asking their bank
// to take back a payment.
//
//	needs_response --(merchant sends evidence, submits)--> under_review --(issuer decides)--> won | lost
//	       |                                                                                    ^
//	       +--(merchant accepts, or no evidence before evidence_due_by)--------------------> lost
//
// The disputed amount leaves the merchant's balance when the dispute opens
// and comes back only if it is won.
package domain

import (
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"paygate/internal/contracts/disputes"
	"paygate/internal/platform/apperror"
)

var transitions = map[string][]string{
	disputes.StatusNeedsResponse: {disputes.StatusUnderReview, disputes.StatusLost},
	disputes.StatusUnderReview:   {disputes.StatusWon, disputes.StatusLost},
}

// Reason codes (card network categories).
var ReasonCodes = []string{"fraudulent", "product_not_received", "product_unacceptable", "duplicate",
	"credit_not_processed", "subscription_canceled", "general"}

const (
	MaxFiles     = 5
	MaxFileBytes = 5 << 20
)

type Dispute struct {
	ID                  string
	PaymentID           string
	MerchantID          string
	Environment         string
	Provider            string
	Amount              int64
	Currency            string
	Status              string
	ReasonCode          string
	Reason              string
	ProviderCaseID      string
	EvidenceDueBy       time.Time
	EvidenceText        string
	EvidenceSubmittedAt *time.Time
	Outcome             string
	Fee                 int64
	Note                string
	ClosedBy            string
	ClosedAt            *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type OpenInput struct {
	PaymentID, MerchantID, Environment, Provider, Currency string
	Amount, Disputable                                     int64 // Disputable: captured minus refunded minus disputed
	ReasonCode, Reason, ProviderCaseID                     string
	EvidenceDueBy                                          time.Time
}

func Open(in OpenInput, now time.Time) (*Dispute, error) {
	switch {
	case in.Amount <= 0:
		return nil, apperror.Invalid("invalid_amount", "amount must be positive")
	case in.Amount > in.Disputable:
		return nil, apperror.Unprocessable("dispute_exceeds_payment", "amount exceeds what is left of the payment (captured - refunded - disputed)")
	case !slices.Contains(ReasonCodes, in.ReasonCode):
		return nil, apperror.Invalid("invalid_reason_code", "reason_code must be one of: "+strings.Join(ReasonCodes, ", "))
	case !in.EvidenceDueBy.After(now):
		return nil, apperror.Invalid("invalid_due_date", "evidence_due_by must be in the future")
	}
	return &Dispute{ID: uuid.NewString(), PaymentID: in.PaymentID, MerchantID: in.MerchantID, Environment: in.Environment,
		Provider: in.Provider, Amount: in.Amount, Currency: in.Currency, Status: disputes.StatusNeedsResponse,
		ReasonCode: in.ReasonCode, Reason: strings.TrimSpace(in.Reason), ProviderCaseID: strings.TrimSpace(in.ProviderCaseID),
		EvidenceDueBy: in.EvidenceDueBy, CreatedAt: now, UpdatedAt: now}, nil
}

func (d *Dispute) to(next string, now time.Time) error {
	if !slices.Contains(transitions[d.Status], next) {
		return apperror.Conflict("invalid_dispute_transition", "this dispute is "+d.Status)
	}
	d.Status, d.UpdatedAt = next, now
	return nil
}

// AddEvidence: the merchant's written explanation, while it can still respond.
func (d *Dispute) AddEvidence(text string, now time.Time) error {
	if d.Status != disputes.StatusNeedsResponse {
		return apperror.Conflict("evidence_closed", "evidence can only be added while the dispute needs a response")
	}
	if !now.Before(d.EvidenceDueBy) {
		return apperror.Conflict("evidence_overdue", "the evidence deadline has passed")
	}
	if t := strings.TrimSpace(text); t != "" {
		if len(t) > 20_000 {
			return apperror.Invalid("evidence_too_long", "evidence text is at most 20000 characters")
		}
		d.EvidenceText = t
	}
	d.UpdatedAt = now
	return nil
}

// Submit sends the evidence for review (no more changes).
func (d *Dispute) Submit(files int, now time.Time) error {
	if d.Status == disputes.StatusNeedsResponse && !now.Before(d.EvidenceDueBy) {
		return apperror.Conflict("evidence_overdue", "the evidence deadline has passed")
	}
	if d.Status == disputes.StatusNeedsResponse && d.EvidenceText == "" && files == 0 {
		return apperror.Invalid("evidence_required", "add evidence (text or files) before submitting")
	}
	if err := d.to(disputes.StatusUnderReview, now); err != nil {
		return err
	}
	d.EvidenceSubmittedAt = &now
	return nil
}

// Close records the outcome.
func (d *Dispute) Close(outcome string, fee int64, note, by string, now time.Time) error {
	if outcome != disputes.OutcomeWon && outcome != disputes.OutcomeLost {
		return apperror.Invalid("invalid_outcome", "outcome must be won or lost")
	}
	if fee < 0 || (outcome == disputes.OutcomeWon && fee > 0) {
		return apperror.Invalid("invalid_fee", "a fee is charged only on a lost dispute")
	}
	if outcome == disputes.OutcomeWon && d.Status != disputes.StatusUnderReview {
		return apperror.Conflict("invalid_dispute_transition", "only a dispute under review can be won")
	}
	if err := d.to(outcome, now); err != nil {
		return err
	}
	d.Outcome, d.Fee, d.Note, d.ClosedBy, d.ClosedAt = outcome, fee, strings.TrimSpace(note), by, &now
	return nil
}

func (d *Dispute) Open() bool {
	return d.Status == disputes.StatusNeedsResponse || d.Status == disputes.StatusUnderReview
}
